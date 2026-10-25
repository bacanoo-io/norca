// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
	"github.com/hashicorp/nomad/secretstore/ubix/export/audit"
	"github.com/hashicorp/nomad/secretstore/ubix/export/barrier"
	"github.com/hashicorp/nomad/secretstore/ubix/export/core"
	"github.com/hashicorp/nomad/secretstore/ubix/export/kv"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
)

// The secrets of the built-in store are kept by uBixVault's KV v2 engine,
// mounted at secretKVPrefix on the barrier of the leader's core, under the
// key "<namespace>/<path>". The engine owns the values, the versions, soft
// delete, undelete, destroy and the max_versions bound; everything it
// writes is encrypted by the barrier.
//
// Consistency of the values and the metadata rows: every write runs on the
// leader under serverSecrets.writeLock and drives the KV engine with a
// context carrying a secretstore.Batch. The barrier's writes are collected in
// the batch instead of being applied one by one, and reads see the batch, so
// the engine reads its own writes. The store then builds the new metadata
// row from the engine's version history and applies the row together with
// the collected ciphertext blobs as one Raft entry (SecretApplyRequestType,
// or SecretPurgeRequestType for a whole secret), which the FSM applies in a
// single state store transaction. The KV data and the row therefore change
// together or not at all:
//
//   - Ordering: nothing is written before the Raft entry, so there is no
//     window in which a reader sees one without the other.
//   - Idempotency and races: the entry carries the ModifyIndex of the row the
//     leader computed it from; the FSM rejects it with a conflict if the row
//     changed since (for example a write of a former leader that committed
//     late), and rejects the creation of the value hash key if it exists.
//     A retried write without a check index writes a new version, as any
//     write does; with a check index it fails with a conflict.
//   - Leader change mid-write: a server that lost leadership can no longer
//     apply (raftApply fails and the HA lock fences its core), so an
//     interrupted write is either committed completely or not at all, and
//     the new leader's core reads the replicated state.

const (
	// secretKVPrefix is the barrier prefix of the KV engine of the secrets.
	secretKVPrefix = "secrets"

	// secretHMACKeyPath is the barrier path of the key of the value hashes.
	secretHMACKeyPath = "norca/secrets-value-hash-key"
)

// serverSecrets is the per-server state of the secrets of the built-in
// store.
type serverSecrets struct {
	// writeLock serializes the writes on the leader, each of which reads
	// the KV metadata and applies a batch computed from it.
	writeLock sync.Mutex

	// audit fans the audit records out to the SecretAccess topic and the
	// audit file. It is fail closed.
	audit *audit.Broker

	// auditFile is the audit file, if configured.
	auditFile *secretstore.AuditFile
}

// setupSecrets creates the audit trail of the secrets.
func (s *Server) setupSecrets() error {
	ss := &serverSecrets{}
	devices := []audit.Device{&secretEventDevice{srv: s}}
	if cfg := s.config.SecretsAudit; cfg != nil && cfg.Path != "" {
		f, err := secretstore.NewAuditFile(*cfg)
		if err != nil {
			return err
		}
		ss.auditFile = f
		devices = append(devices, f)
	}
	ss.audit = audit.NewBroker(devices...)
	s.secrets = ss
	return nil
}

// shutdownSecrets closes the audit file.
func (s *Server) shutdownSecrets() {
	if s.secrets == nil {
		return
	}
	if err := s.secrets.audit.Close(); err != nil {
		s.logger.Warn("failed to close the secret store audit file", "error", err)
	}
}

// secretKVKey is the KV engine key of a secret.
func secretKVKey(namespace, path string) string {
	return namespace + "/" + path
}

// secretKVMetaKey is the barrier key of the KV engine's metadata of a
// secret, as the engine stores it.
func secretKVMetaKey(namespace, path string) string {
	return secretKVPrefix + "/meta/" + secretKVKey(namespace, path)
}

// errSecretUnavailable reports that the secret store cannot serve values or
// writes on this server (sealed, not initialized or not active).
func errSecretUnavailable(err error) error {
	return structs.NewErrRPCCodedf(http.StatusServiceUnavailable, "secret store unavailable: %v", err)
}

// secretCodedError converts the errors of the store's operations to coded
// RPC errors.
func secretCodedError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, structs.ErrSecretConflict):
		return structs.NewErrRPCCoded(http.StatusConflict, structs.ErrSecretConflict.Error())
	case errors.Is(err, structs.ErrSecretNotFound), errors.Is(err, kv.ErrSecretNotFound):
		return structs.NewErrRPCCoded(http.StatusNotFound, structs.ErrSecretNotFound.Error())
	case errors.Is(err, kv.ErrVersionNotFound):
		return structs.NewErrRPCCoded(http.StatusNotFound, "secret version not found")
	case errors.Is(err, kv.ErrVersionDeleted):
		return structs.NewErrRPCCoded(http.StatusNotFound, "secret version is deleted")
	case errors.Is(err, kv.ErrVersionDestroyed):
		return structs.NewErrRPCCoded(http.StatusNotFound, "secret version is destroyed")
	case errors.Is(err, barrier.ErrSealed), errors.Is(err, storage.ErrFenced):
		return errSecretUnavailable(err)
	case errors.Is(err, secretstore.ErrServersNotReady):
		return structs.NewErrRPCCoded(http.StatusServiceUnavailable, err.Error())
	default:
		return err
	}
}

// secretAccessResult is the audit result of an operation's error.
func secretAccessResult(err error) string {
	if err == nil {
		return structs.SecretAccessResultOK
	}
	code, _, ok := structs.CodeFromRPCCodedErr(secretCodedError(err))
	switch {
	case errors.Is(err, structs.ErrPermissionDenied):
		return structs.SecretAccessResultDenied
	case !ok:
		return structs.SecretAccessResultError
	case code == http.StatusNotFound:
		return structs.SecretAccessResultNotFound
	case code == http.StatusConflict:
		return structs.SecretAccessResultConflict
	case code == http.StatusServiceUnavailable:
		return structs.SecretAccessResultSealed
	case code == http.StatusBadRequest:
		return structs.SecretAccessResultInvalid
	default:
		return structs.SecretAccessResultError
	}
}

// secretValueHashKey returns the key of the value hashes from the barrier.
// If there is none yet, a new key is written to the batch of ctx and
// created is true.
func secretValueHashKey(ctx context.Context, b *barrier.Barrier) (key []byte, created bool, err error) {
	e, err := b.Get(ctx, secretHMACKeyPath)
	if err != nil {
		return nil, false, err
	}
	if e != nil && len(e.Value) > 0 {
		return e.Value, false, nil
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, false, err
	}
	if err := b.Put(ctx, &storage.Entry{Key: secretHMACKeyPath, Value: key}); err != nil {
		return nil, false, err
	}
	return key, true, nil
}

// secretValueHash is the keyed hash of a version's items, bound to the
// secret's namespace and path. Without the key, which only exists inside the
// barrier, a value cannot be guessed from its hash.
func secretValueHash(key []byte, namespace, path string, items structs.SecretItems) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(namespace))
	mac.Write([]byte{0})
	mac.Write([]byte(path))
	mac.Write([]byte{0})
	// encoding/json writes the keys of a map sorted.
	data, _ := json.Marshal(map[string]string(items))
	mac.Write(data)
	return structs.SecretValueHashPrefix + hex.EncodeToString(mac.Sum(nil))
}

// checkSecretIndex applies the check-and-set index of a write.
func checkSecretIndex(existing *structs.Secret, checkIndex *uint64) error {
	if checkIndex == nil {
		return nil
	}
	var current uint64
	if existing != nil {
		current = existing.ModifyIndex
	}
	if current != *checkIndex {
		return structs.ErrSecretConflict
	}
	return nil
}

// setSecretMaxVersions stores the max_versions bound in the KV engine's
// metadata of the secret, which the engine applies on its next write. The
// engine has no setter for it; its metadata format is its exported
// kv.Metadata type.
func setSecretMaxVersions(ctx context.Context, b *barrier.Barrier, eng *kv.Engine, namespace, path string, max int) error {
	meta, err := eng.ReadMetadata(ctx, secretKVKey(namespace, path))
	switch {
	case errors.Is(err, kv.ErrSecretNotFound):
		meta = &kv.Metadata{Versions: map[int]*kv.VersionMeta{}}
	case err != nil:
		return err
	case meta.MaxVersions == max:
		return nil
	}
	meta.MaxVersions = max
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return b.Put(ctx, &storage.Entry{Key: secretKVMetaKey(namespace, path), Value: data})
}

// secretRow builds the metadata row of a secret from the KV engine's
// version history. The value hashes of retained versions are taken from the
// existing row; the caller sets the hash of a new version.
func secretRow(existing *structs.Secret, namespace, path string, meta structs.SecretMeta, kvMeta *kv.Metadata, now time.Time) *structs.Secret {
	row := &structs.Secret{
		Namespace:      namespace,
		Path:           path,
		SecretMeta:     meta,
		CurrentVersion: uint64(kvMeta.CurrentVersion),
		CreateTime:     now.UnixNano(),
		ModifyTime:     now.UnixNano(),
	}
	if existing != nil {
		row.CreateTime = existing.CreateTime
		row.ItemKeys = append([]string(nil), existing.ItemKeys...)
	}

	numbers := make([]int, 0, len(kvMeta.Versions))
	for v := range kvMeta.Versions {
		numbers = append(numbers, v)
	}
	sort.Ints(numbers)
	for _, v := range numbers {
		vm := kvMeta.Versions[v]
		sv := &structs.SecretVersion{
			Version:    uint64(v),
			CreateTime: vm.CreatedTime.UnixNano(),
			Deleted:    vm.Deleted,
			Destroyed:  vm.Destroyed,
		}
		if existing != nil && !vm.Destroyed {
			if old := existing.Version(sv.Version); old != nil {
				sv.ValueHash = old.ValueHash
			}
		}
		row.Versions = append(row.Versions, sv)
	}
	secretRowCurrent(row)
	return row
}

// secretRowCurrent copies the state of the current version to the row.
func secretRowCurrent(row *structs.Secret) {
	row.Deleted, row.Destroyed, row.ValueHash = false, false, ""
	if cur := row.Version(row.CurrentVersion); cur != nil {
		row.Deleted, row.Destroyed, row.ValueHash = cur.Deleted, cur.Destroyed, cur.ValueHash
	}
}

// lockedSecretCore returns the active core and takes the write lock; the
// caller must call the returned unlock function.
func (s *Server) lockedSecretCore() (*core.Core, func(), error) {
	c, err := s.secretCore()
	if err != nil {
		return nil, nil, errSecretUnavailable(err)
	}
	s.secrets.writeLock.Lock()
	return c, s.secrets.writeLock.Unlock, nil
}

// secretUpsert writes a new version of a secret and/or its metadata on the
// leader's core.
func (s *Server) secretUpsert(args *structs.SecretUpsertRequest) (*structs.Secret, uint64, error) {
	return s.secretWrite(&secretWrite{
		namespace:  args.RequestNamespace(),
		path:       args.Path,
		items:      args.Items,
		meta:       args.Meta,
		checkIndex: args.CheckIndex,
	})
}

// secretWrite is a write of a new version of a secret and/or its metadata.
type secretWrite struct {
	namespace, path string
	items           structs.SecretItems

	// meta replaces the metadata; nil keeps the current metadata, or uses
	// metaIfNew if the secret does not exist.
	meta      *structs.SecretMeta
	metaIfNew *structs.SecretMeta

	checkIndex *uint64
}

// secretWrite writes a new version of a secret and/or its metadata on the
// leader's core. Every write of a value goes through it: the secret API and
// the selective restore of a secret from a backup.
func (s *Server) secretWrite(args *secretWrite) (*structs.Secret, uint64, error) {
	c, unlock, err := s.lockedSecretCore()
	if err != nil {
		return nil, 0, err
	}
	defer unlock()

	ns, path := args.namespace, args.path
	existing, err := s.fsm.State().SecretByPath(nil, ns, path)
	if err != nil {
		return nil, 0, err
	}
	if err := checkSecretIndex(existing, args.checkIndex); err != nil {
		return nil, 0, err
	}
	if len(args.items) == 0 && existing == nil {
		return nil, 0, structs.ErrSecretNotFound
	}

	var meta structs.SecretMeta
	switch {
	case args.meta != nil:
		meta = *args.meta.Copy()
	case existing != nil:
		meta = *existing.SecretMeta.Copy()
	case args.metaIfNew != nil:
		meta = *args.metaIfNew.Copy()
	}
	meta.Canonicalize()

	ctx, batch := secretstore.WithBatch(context.Background())
	b := c.Barrier()
	eng := kv.New(b, secretKVPrefix)
	key := secretKVKey(ns, path)

	hashKey, created, err := secretValueHashKey(ctx, b)
	if err != nil {
		return nil, 0, err
	}
	if err := setSecretMaxVersions(ctx, b, eng, ns, path, meta.MaxVersions); err != nil {
		return nil, 0, err
	}
	var written *kv.VersionMeta
	if len(args.items) > 0 {
		data := make(map[string]any, len(args.items))
		for k, v := range args.items {
			data[k] = v
		}
		if written, err = eng.Write(ctx, key, data); err != nil {
			return nil, 0, err
		}
	}
	kvMeta, err := eng.ReadMetadata(ctx, key)
	if err != nil {
		return nil, 0, err
	}

	row := secretRow(existing, ns, path, meta, kvMeta, time.Now())
	if written != nil {
		if sv := row.Version(uint64(written.Version)); sv != nil {
			sv.ValueHash = secretValueHash(hashKey, ns, path, args.items)
		}
		row.ItemKeys = args.items.Keys()
		secretRowCurrent(row)
	}

	req := &structs.SecretApplyRequest{
		Secret:      row,
		Blobs:       batch.Blobs(),
		DeleteBlobs: batch.Deletes(),
	}
	if existing != nil {
		req.ExpectedIndex = existing.ModifyIndex
	}
	if created {
		req.CreateBlobs = []string{secretHMACKeyPath}
	}
	return s.secretApply(req)
}

// secretVersionsOp soft deletes, undeletes or destroys versions of a secret
// on the leader's core. Destroy without versions removes the whole secret
// and returns a nil secret.
func (s *Server) secretVersionsOp(op string, args *structs.SecretVersionsRequest) (*structs.Secret, uint64, error) {
	c, unlock, err := s.lockedSecretCore()
	if err != nil {
		return nil, 0, err
	}
	defer unlock()

	ns, path := args.RequestNamespace(), args.Path
	existing, err := s.fsm.State().SecretByPath(nil, ns, path)
	if err != nil {
		return nil, 0, err
	}
	if existing == nil {
		return nil, 0, structs.ErrSecretNotFound
	}
	if err := checkSecretIndex(existing, args.CheckIndex); err != nil {
		return nil, 0, err
	}
	versions := make([]int, 0, len(args.Versions))
	for _, v := range args.Versions {
		if existing.Version(v) == nil || v > math.MaxInt32 {
			return nil, 0, structs.NewErrRPCCodedf(http.StatusNotFound, "secret version %d not found", v)
		}
		versions = append(versions, int(v))
	}

	ctx, batch := secretstore.WithBatch(context.Background())
	eng := kv.New(c.Barrier(), secretKVPrefix)
	key := secretKVKey(ns, path)

	switch op {
	case structs.SecretOpDelete:
		err = eng.Delete(ctx, key, versions...)
	case structs.SecretOpUndelete:
		err = eng.Undelete(ctx, key, versions...)
	case structs.SecretOpDestroy:
		if len(versions) == 0 {
			if err := eng.DeleteMetadata(ctx, key); err != nil {
				return nil, 0, err
			}
			return s.secretPurge(&structs.SecretPurgeRequest{
				Namespace:     ns,
				Path:          path,
				ExpectedIndex: existing.ModifyIndex,
				DeleteBlobs:   batch.Deletes(),
			})
		}
		err = eng.Destroy(ctx, key, versions...)
	default:
		return nil, 0, fmt.Errorf("unknown secret operation %q", op)
	}
	if err != nil {
		return nil, 0, err
	}
	kvMeta, err := eng.ReadMetadata(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	row := secretRow(existing, ns, path, existing.SecretMeta, kvMeta, time.Now())
	return s.secretApply(&structs.SecretApplyRequest{
		Secret:        row,
		ExpectedIndex: existing.ModifyIndex,
		Blobs:         batch.Blobs(),
		DeleteBlobs:   batch.Deletes(),
	})
}

// secretApply applies a secret write through Raft and returns the written
// row.
func (s *Server) secretApply(req *structs.SecretApplyRequest) (*structs.Secret, uint64, error) {
	if err := s.secretStore.CheckWrite(context.Background()); err != nil {
		return nil, 0, err
	}
	resp, index, err := s.raftApply(structs.SecretApplyRequestType|structs.IgnoreUnknownTypeFlag, req)
	if err != nil {
		return nil, 0, err
	}
	if err, ok := resp.(error); ok && err != nil {
		return nil, 0, err
	}
	row, err := s.fsm.State().SecretByPath(nil, req.Secret.Namespace, req.Secret.Path)
	if err != nil {
		return nil, 0, err
	}
	return row.Copy(), index, nil
}

// secretPurge removes a secret through Raft.
func (s *Server) secretPurge(req *structs.SecretPurgeRequest) (*structs.Secret, uint64, error) {
	if err := s.secretStore.CheckWrite(context.Background()); err != nil {
		return nil, 0, err
	}
	resp, index, err := s.raftApply(structs.SecretPurgeRequestType|structs.IgnoreUnknownTypeFlag, req)
	if err != nil {
		return nil, 0, err
	}
	if err, ok := resp.(error); ok && err != nil {
		return nil, 0, err
	}
	return nil, index, nil
}

// secretReadValue reads the items of a version (0: the current one) of a
// secret from the leader's core.
func (s *Server) secretReadValue(namespace, path string, version uint64) (structs.SecretItems, uint64, error) {
	c, err := s.secretCore()
	if err != nil {
		return nil, 0, errSecretUnavailable(err)
	}
	if version > math.MaxInt32 {
		return nil, 0, kv.ErrVersionNotFound
	}
	eng := kv.New(c.Barrier(), secretKVPrefix)
	data, vm, err := eng.Read(context.Background(), secretKVKey(namespace, path), int(version))
	if err != nil {
		return nil, 0, err
	}
	items := make(structs.SecretItems, len(data))
	for k, v := range data {
		s, ok := v.(string)
		if !ok {
			return nil, 0, fmt.Errorf("secret item %q is not a string", k)
		}
		items[k] = s
	}
	return items, uint64(vm.Version), nil
}

// secretEventDevice is the audit device that publishes the response records
// on the SecretAccess topic of this server's event broker. Records are not
// written to Raft: they are published by the server that executed the
// access, which is the leader for value reads and writes.
type secretEventDevice struct {
	srv *Server
}

func (d *secretEventDevice) Log(ctx context.Context, e *audit.Entry) error {
	rec := secretstore.AuditRecordFrom(ctx)
	if rec == nil || e.Type != "response" {
		return nil
	}
	store := d.srv.State()
	if store == nil {
		return nil
	}
	broker, err := store.EventBroker()
	if err != nil {
		// The event broker is disabled.
		return nil
	}
	index, _ := store.LatestIndex()
	payload := *rec
	payload.Versions = append([]uint64(nil), rec.Versions...)
	if rec.Accessor != nil {
		acc := *rec.Accessor
		payload.Accessor = &acc
	}
	broker.Publish(&structs.Events{
		Index: index,
		Events: []structs.Event{{
			Topic:     structs.TopicSecretAccess,
			Type:      structs.TypeSecretAccessed,
			Key:       rec.Path,
			Namespace: rec.Namespace,
			Index:     index,
			Payload:   &payload,
		}},
	})
	return nil
}

func (d *secretEventDevice) Close() error { return nil }

// secretAudit records an access: phase "request" before the access is
// executed, "response" with its result. The broker is fail closed: an
// error means the access must be refused (or its value withheld).
func (s *Server) secretAudit(rec *structs.SecretAccessEvent, response bool) error {
	rec.Time = time.Now().UTC()
	ctx := secretstore.WithAuditRecord(context.Background(), rec)
	entry := &audit.Entry{
		Time:      rec.Time,
		Operation: rec.Operation,
		Path:      rec.Namespace + "/" + rec.Path,
	}
	if rec.Accessor != nil {
		entry.RemoteAddr = rec.Accessor.RemoteIP
	}
	var err error
	if response {
		err = s.secrets.audit.LogResponse(ctx, entry)
	} else {
		err = s.secrets.audit.LogRequest(ctx, entry)
	}
	if err != nil {
		s.logger.Error("failed to record a secret store audit record",
			"namespace", rec.Namespace, "path", rec.Path, "operation", rec.Operation, "error", err)
		return structs.NewErrRPCCodedf(http.StatusInternalServerError, "secret store audit failed: %v", err)
	}
	return nil
}
