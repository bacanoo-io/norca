// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
	"github.com/shoenig/test/must"
)

// secretBlobs returns the barrier blobs of a secret in a state store: the KV
// metadata and the data of its versions, but not those of secrets below its
// path.
func secretBlobs(store *state.StateStore, namespace, path string) ([]*structs.SecretStoreBlob, error) {
	var out []*structs.SecretStoreBlob
	meta, err := store.SecretStoreBlobByPath(nil, secretKVMetaKey(namespace, path))
	if err != nil {
		return nil, err
	}
	if meta != nil {
		out = append(out, meta)
	}
	prefix := secretKVDataPrefix(namespace, path)
	iter, err := store.SecretStoreBlobsByPrefix(nil, prefix)
	if err != nil {
		return nil, err
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		blob := raw.(*structs.SecretStoreBlob)
		if _, err := strconv.ParseUint(strings.TrimPrefix(blob.Path, prefix), 10, 64); err != nil {
			// A blob of a secret below this path.
			continue
		}
		out = append(out, blob)
	}
	return out, nil
}

// secretRestoreTest is a server with a secret restorer over a backup taken
// with snapshot.
type secretRestoreTest struct {
	t      *testing.T
	s      *Server
	backup *state.StateStore
}

// snapshot takes the "backup" of the server's state.
func (st *secretRestoreTest) snapshot() {
	st.backup = testSnapshotRestore(st.t, st.s.fsm).State()
}

// restore restores an item from the backup with the token.
func (st *secretRestoreTest) restore(item *structs.BackupRestoreItem, token string, dryRun bool) *structs.BackupRestoreItemResult {
	st.t.Helper()
	must.NoError(st.t, validateBackupRestoreItem(item))
	r := &backupRestorer{
		b: &Backup{srv: st.s},
		args: &structs.BackupRestoreRequest{
			DryRun:       dryRun,
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token},
		},
		loaded: &backup.Loaded{Record: &structs.BackupRecord{ID: "b1"}, State: st.backup},
	}
	// The Backup.Restore RPC authenticates the request first.
	must.NoError(st.t, st.s.Authenticate(nil, r.args))
	return r.restore(item)
}

func (st *secretRestoreTest) write(path string, items structs.SecretItems, meta *structs.SecretMeta) *structs.Secret {
	st.t.Helper()
	req := secretWriteReq(path, "", items)
	req.Meta = meta
	var w structs.SecretWriteResponse
	must.NoError(st.t, st.s.RPC(structs.SecretUpsertRPCMethod, req, &w))
	return w.Secret
}

func (st *secretRestoreTest) metadata(path string) *structs.Secret {
	st.t.Helper()
	var m structs.SecretMetadataResponse
	must.NoError(st.t, st.s.RPC(structs.SecretReadMetadataRPCMethod,
		&structs.SecretReadMetadataRequest{Path: path, QueryOptions: secretQuery("")}, &m))
	return m.Secret
}

func (st *secretRestoreTest) read(path string, version uint64) structs.SecretItems {
	st.t.Helper()
	var r structs.SecretReadResponse
	must.NoError(st.t, st.s.RPC(structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: path, Version: version, QueryOptions: secretQuery("")}, &r))
	return r.Items
}

func secretItem(path string) *structs.BackupRestoreItem {
	return &structs.BackupRestoreItem{Type: structs.BackupRestoreItemSecret, Namespace: "default", ID: path}
}

// TestSecretRestore_NewVersion restores the value of a backup's version as a
// new version on top of the current history.
func TestSecretRestore_NewVersion(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()
	st := &secretRestoreTest{t: t, s: s}

	for v := 1; v <= 5; v++ {
		st.write("app/db", structs.SecretItems{"k": fmt.Sprintf("v%d", v)}, nil)
	}
	st.write("app/db/child", structs.SecretItems{"c": "c"}, nil)
	st.snapshot()
	backupRow, err := st.backup.SecretByPath(nil, "default", "app/db")
	must.NoError(t, err)
	st.write("app/db", structs.SecretItems{"k": "v6"}, nil)
	current := st.write("app/db", structs.SecretItems{"k": "v7", "extra": "x"}, nil)
	must.Eq(t, 7, current.CurrentVersion)

	// A dry run shows the version it would write, and writes nothing.
	result := st.restore(secretItem("app/db"), "", true)
	must.Eq(t, "", result.Error)
	must.Eq(t, "update", result.Action)
	must.Eq(t, 5, result.BackupVersion)
	must.Eq(t, 8, result.NewVersion)
	must.Eq(t, current.ModifyIndex, result.CurrentIndex)
	must.Zero(t, result.Index)
	must.StrContains(t, result.Description, "version 5")
	must.StrContains(t, result.Description, "backup b1")
	must.Eq(t, 7, st.metadata("app/db").CurrentVersion)

	// The backup's latest version (5) becomes version 8.
	result = st.restore(secretItem("app/db"), "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 5, result.BackupVersion)
	must.Eq(t, 8, result.NewVersion)
	must.Positive(t, result.Index)
	row := st.metadata("app/db")
	must.Eq(t, 8, row.CurrentVersion)
	must.Len(t, 8, row.Versions)
	must.Eq(t, []string{"k"}, row.ItemKeys)
	must.Eq(t, backupRow.Version(5).ValueHash, row.ValueHash)
	must.Eq(t, structs.SecretItems{"k": "v5"}, st.read("app/db", 8))
	for v := uint64(1); v <= 6; v++ {
		must.Eq(t, structs.SecretItems{"k": fmt.Sprintf("v%d", v)}, st.read("app/db", v))
	}
	must.Eq(t, structs.SecretItems{"k": "v7", "extra": "x"}, st.read("app/db", 7))

	// Another version of the backup.
	item := secretItem("app/db")
	item.Version = pointer.Of(uint64(2))
	result = st.restore(item, "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 2, result.BackupVersion)
	must.Eq(t, 9, result.NewVersion)
	must.Eq(t, structs.SecretItems{"k": "v2"}, st.read("app/db", 0))

	// A version that is not in the backup.
	item.Version = pointer.Of(uint64(6))
	result = st.restore(item, "", false)
	must.StrContains(t, result.Error, "version 6 of secret \"app/db\" is not in the backup")
	must.StrContains(t, result.Error, "1, 2, 3, 4, 5")
	must.Zero(t, result.NewVersion)
	must.Eq(t, 9, st.metadata("app/db").CurrentVersion)

	// A secret that is not in the backup.
	result = st.restore(secretItem("other"), "", false)
	must.StrContains(t, result.Error, "not found in the backup")

	// The secret below the restored path is untouched.
	must.Eq(t, structs.SecretItems{"c": "c"}, st.read("app/db/child", 0))
	must.Eq(t, 1, st.metadata("app/db/child").CurrentVersion)
}

// TestSecretRestore_Metadata restores the value only by default, and the
// backup's metadata with WithMetadata or for a secret that does not exist.
func TestSecretRestore_Metadata(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()
	st := &secretRestoreTest{t: t, s: s}

	backupMeta := &structs.SecretMeta{Description: "old", Owner: "team-a", Labels: map[string]string{"tier": "a"},
		RotateEvery: time.Hour, ExpiresAt: pointer.Of(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)), MaxVersions: 5}
	st.write("app/db", structs.SecretItems{"k": "one"}, backupMeta)
	st.snapshot()
	currentMeta := &structs.SecretMeta{Description: "new", Owner: "team-b", MaxVersions: 7}
	st.write("app/db", structs.SecretItems{"k": "two"}, currentMeta)

	// By default the current metadata is kept.
	result := st.restore(secretItem("app/db"), "", false)
	must.Eq(t, "", result.Error)
	must.StrContains(t, result.Description, "current metadata is kept")
	row := st.metadata("app/db")
	must.Eq(t, 3, row.CurrentVersion)
	must.Eq(t, "new", row.Description)
	must.Eq(t, "team-b", row.Owner)
	must.Eq(t, 7, row.MaxVersions)
	must.Nil(t, row.ExpiresAt)
	must.Eq(t, structs.SecretItems{"k": "one"}, st.read("app/db", 0))

	// With the metadata of the backup.
	item := secretItem("app/db")
	item.WithMetadata = true
	result = st.restore(item, "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 4, result.NewVersion)
	row = st.metadata("app/db")
	must.Eq(t, 4, row.CurrentVersion)
	must.Eq(t, "old", row.Description)
	must.Eq(t, "team-a", row.Owner)
	must.Eq(t, map[string]string{"tier": "a"}, row.Labels)
	must.Eq(t, time.Hour, row.RotateEvery)
	must.Eq(t, 5, row.MaxVersions)
	must.NotNil(t, row.ExpiresAt)

	// A secret that no longer exists is created with the backup's metadata,
	// with or without the option.
	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretDestroyRPCMethod, secretVersionsReq("app/db", ""), &w))
	must.Nil(t, st.metadata("app/db"))
	result = st.restore(secretItem("app/db"), "", true)
	must.Eq(t, "", result.Error)
	must.Eq(t, "create", result.Action)
	must.Eq(t, 1, result.NewVersion)
	result = st.restore(secretItem("app/db"), "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, "create", result.Action)
	must.Eq(t, 1, result.NewVersion)
	row = st.metadata("app/db")
	must.Eq(t, 1, row.CurrentVersion)
	must.Eq(t, "team-a", row.Owner)
	must.Eq(t, 5, row.MaxVersions)
	must.Eq(t, structs.SecretItems{"k": "one"}, st.read("app/db", 0))

	// The option is only accepted for secrets.
	must.ErrorContains(t, validateBackupRestoreItem(&structs.BackupRestoreItem{
		Type: structs.BackupRestoreItemVariable, ID: "x", WithMetadata: true}), "only used for secrets")
	must.ErrorContains(t, validateBackupRestoreItem(&structs.BackupRestoreItem{
		Type: structs.BackupRestoreItemVariable, ID: "x", Version: pointer.Of(uint64(1))}), "jobs and secrets")
}

// TestSecretRestore_DeletedTargets restores over a soft deleted secret and
// a secret whose current version is destroyed.
func TestSecretRestore_DeletedTargets(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()
	st := &secretRestoreTest{t: t, s: s}

	st.write("app/db", structs.SecretItems{"k": "one"}, nil)
	st.write("app/db", structs.SecretItems{"k": "two"}, nil)
	st.snapshot()

	// Soft deleted: the new version makes it live again.
	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretDeleteRPCMethod, secretVersionsReq("app/db", ""), &w))
	must.True(t, w.Secret.Deleted)
	result := st.restore(secretItem("app/db"), "", true)
	must.StrContains(t, result.Description, "becomes live again")
	result = st.restore(secretItem("app/db"), "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 3, result.NewVersion)
	row := st.metadata("app/db")
	must.False(t, row.Deleted)
	must.True(t, row.Version(2).Deleted)
	must.Eq(t, structs.SecretItems{"k": "two"}, st.read("app/db", 0))

	// The current version destroyed.
	must.NoError(t, s.RPC(structs.SecretDestroyRPCMethod, secretVersionsReq("app/db", "", 3), &w))
	must.True(t, w.Secret.Destroyed)
	item := secretItem("app/db")
	item.Version = pointer.Of(uint64(1))
	result = st.restore(item, "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 4, result.NewVersion)
	row = st.metadata("app/db")
	must.False(t, row.Destroyed)
	must.True(t, row.Version(3).Destroyed)
	must.Eq(t, structs.SecretItems{"k": "one"}, st.read("app/db", 0))

	// A version destroyed in the backup can not be restored.
	must.NoError(t, s.RPC(structs.SecretDestroyRPCMethod, secretVersionsReq("app/db", "", 1), &w))
	st.snapshot()
	result = st.restore(item, "", false)
	must.StrContains(t, result.Error, "destroyed in the backup")
}

// TestSecretRestore_CheckIndex checks the optional check-and-set guard.
func TestSecretRestore_CheckIndex(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()
	st := &secretRestoreTest{t: t, s: s}

	st.write("app/db", structs.SecretItems{"k": "one"}, nil)
	st.snapshot()
	current := st.write("app/db", structs.SecretItems{"k": "two"}, nil)

	item := secretItem("app/db")
	item.CheckIndex = pointer.Of(current.ModifyIndex - 1)
	result := st.restore(item, "", true)
	must.Eq(t, "", result.Error)
	must.StrContains(t, result.Description, "does not match")
	result = st.restore(item, "", false)
	must.StrContains(t, result.Error, "check index mismatch")
	must.Zero(t, result.NewVersion)
	must.Eq(t, 2, st.metadata("app/db").CurrentVersion)

	// 0: only if the secret does not exist.
	item.CheckIndex = pointer.Of(uint64(0))
	result = st.restore(item, "", false)
	must.StrContains(t, result.Error, "check index mismatch")

	item.CheckIndex = pointer.Of(current.ModifyIndex)
	result = st.restore(item, "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 3, result.NewVersion)

	// Without a check index the value is always appended.
	result = st.restore(secretItem("app/db"), "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 4, result.NewVersion)
}

// TestSecretRestore_MaxVersions checks that a restore ages out the oldest
// versions as any write, and that a version aged out of the store can be
// restored from the backup.
func TestSecretRestore_MaxVersions(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()
	st := &secretRestoreTest{t: t, s: s}

	meta := &structs.SecretMeta{MaxVersions: 3}
	for v := 1; v <= 3; v++ {
		st.write("app/db", structs.SecretItems{"k": fmt.Sprintf("v%d", v)}, meta)
	}
	st.snapshot()
	st.write("app/db", structs.SecretItems{"k": "v4"}, nil)

	item := secretItem("app/db")
	item.Version = pointer.Of(uint64(1))
	result := st.restore(item, "", false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 5, result.NewVersion)
	row := st.metadata("app/db")
	var versions []uint64
	for _, sv := range row.Versions {
		versions = append(versions, sv.Version)
	}
	must.Eq(t, []uint64{3, 4, 5}, versions)
	must.Eq(t, structs.SecretItems{"k": "v1"}, st.read("app/db", 5))
	blob, err := s.State().SecretStoreBlobByPath(nil, secretKVDataPrefix("default", "app/db")+"2")
	must.NoError(t, err)
	must.Nil(t, blob)
}

// TestSecretRestore_OtherStore restores from the backup of another region's
// store, whose data the store can not decrypt.
func TestSecretRestore_OtherStore(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()
	other, _, cleanupOther := secretTestServer(t, false, nil)
	defer cleanupOther()

	var w structs.SecretWriteResponse
	must.NoError(t, other.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("app/db", "", structs.SecretItems{"k": "v"}), &w))
	st := &secretRestoreTest{t: t, s: s, backup: testSnapshotRestore(t, other.fsm).State()}

	for _, dryRun := range []bool{true, false} {
		result := st.restore(secretItem("app/db"), "", dryRun)
		must.StrContains(t, result.Error, "can not be decrypted")
		must.StrContains(t, result.Error, "age copy of the backup")
		must.StrContains(t, result.Error, "recovery unseal")
	}
	must.Nil(t, st.metadata("app/db"))
}

// TestSecretRestore_ACLAuditAndRedaction checks the permission, the audit
// records and the Secret event of a restore, and that the restored value
// appears in none of the outputs.
func TestSecretRestore_ACLAuditAndRedaction(t *testing.T) {
	ci.Parallel(t)
	auditPath := filepath.Join(t.TempDir(), "secrets-audit.log")
	logs := &syncBuffer{}
	s, root, cleanup := secretTestServer(t, true, func(c *Config) {
		c.SecretsAudit = &secretstore.AuditFileConfig{Path: auditPath}
		c.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{
			Name: "server", Level: hclog.Trace, Output: logs,
		})
	})
	defer cleanup()
	st := &secretRestoreTest{t: t, s: s}
	store := s.fsm.State()
	reader := mock.CreatePolicyAndToken(t, store, 1001, "read-only",
		`namespace "default" { capabilities = ["list-secrets", "read-secret"] }`)

	broker, err := store.EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecret: {"*"}, structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod,
		secretWriteReq("app/db", root.SecretID, structs.SecretItems{"password": secretCanary}), &w))
	st.snapshot()
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod,
		secretWriteReq("app/db", root.SecretID, structs.SecretItems{"password": "other"}), &w))

	// Without write-secret the restore is denied, and audited.
	denied := st.restore(secretItem("app/db"), reader.SecretID, false)
	must.Eq(t, structs.ErrPermissionDenied.Error(), denied.Error)
	result := st.restore(secretItem("app/db"), root.SecretID, false)
	must.Eq(t, "", result.Error)
	must.Eq(t, 3, result.NewVersion)
	must.Eq(t, 1, result.BackupVersion)

	var r structs.SecretReadResponse
	must.NoError(t, s.RPC(structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "app/db", QueryOptions: secretQuery(root.SecretID)}, &r))
	must.Eq(t, secretCanary, r.Items["password"])
	must.Eq(t, 3, r.Version)

	// All events of the test, up to the access record of the read above;
	// then those of the restores and the read.
	var allEvents, secretEvents, accessEvents []structs.Event
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for done := false; !done; {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			allEvents = append(allEvents, e)
			switch e.Topic {
			case structs.TopicSecret:
				if e.Payload.(*structs.SecretEvent).Secret.CurrentVersion == 3 {
					secretEvents = append(secretEvents, e)
				}
			case structs.TopicSecretAccess:
				rec := e.Payload.(*structs.SecretAccessEvent)
				if rec.Operation == structs.SecretOpRestore || rec.Version == 3 {
					accessEvents = append(accessEvents, e)
				}
				done = done || rec.Operation == structs.SecretOpRead
			}
		}
	}
	must.Len(t, 1, secretEvents)
	must.Eq(t, structs.TypeSecretUpserted, secretEvents[0].Type)
	must.Eq(t, "app/db", secretEvents[0].Key)

	var results []string
	for _, e := range accessEvents {
		rec := e.Payload.(*structs.SecretAccessEvent)
		results = append(results, rec.Operation+":"+rec.Result)
		if rec.Operation == structs.SecretOpRestore {
			must.Eq(t, "b1", rec.BackupID)
		}
		if rec.Operation == structs.SecretOpRestore && rec.Result == structs.SecretAccessResultOK {
			must.Eq(t, 1, rec.BackupVersion)
		}
	}
	must.Eq(t, []string{"restore:denied", "restore:ok", "read:ok"}, results)
	must.Eq(t, reader.AccessorID, accessEvents[0].Payload.(*structs.SecretAccessEvent).Accessor.AccessorID)
	must.Eq(t, 3, accessEvents[1].Payload.(*structs.SecretAccessEvent).Version)

	// The audit file names the backup and its version.
	f, err := os.Open(auditPath)
	must.NoError(t, err)
	defer f.Close()
	var restoreLines []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line map[string]any
		must.NoError(t, json.Unmarshal(scanner.Bytes(), &line))
		if line["operation"] == structs.SecretOpRestore {
			restoreLines = append(restoreLines, line)
		}
	}
	// The denied response, the request and the response of the restore.
	must.Len(t, 3, restoreLines)
	must.Eq[any](t, "denied", restoreLines[0]["result"])
	for i, line := range restoreLines {
		must.Eq[any](t, "b1", line["backup"])
		if i > 0 {
			must.Eq[any](t, float64(1), line["backup_version"])
		}
	}
	must.Eq[any](t, "ok", restoreLines[2]["result"])
	must.Eq[any](t, float64(3), restoreLines[2]["version"])

	for i, out := range []any{denied, result, allEvents} {
		data, err := json.Marshal(out)
		must.NoError(t, err)
		must.StrNotContains(t, string(data), secretCanary, must.Sprintf("output %d", i))
	}
	audit, err := os.ReadFile(auditPath)
	must.NoError(t, err)
	must.StrNotContains(t, string(audit), secretCanary)
	must.Positive(t, len(logs.String()))
	must.StrNotContains(t, logs.String(), secretCanary)
	assertSecretStoreCiphertext(t, s, secretCanary)
}
