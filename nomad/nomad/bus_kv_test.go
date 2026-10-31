// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/backup/s3fake"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// TestBusKV_Backup checks the Consul KV bucket of the bus in the backups:
// saved with a backup, listed, compared and restored key by key.
func TestBusKV_Backup(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	srv, cleanup := TestServer(t, func(c *Config) {
		enableBus(t)(c)
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.NumSchedulers = 0
		c.Backup = testBackupConfig(s3, identity, false)
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)

	// The leader creates the bucket; the keys are written as the clients
	// do, with their NATS keys.
	js := busJS(t, srv)
	var kv jetstream.KeyValue
	waitBus(t, func() (bool, error) {
		kv, err = js.KeyValue(t.Context(), bus.ConsulKVBucket)
		return err == nil, err
	})
	put := func(key, value string) {
		_, err := kv.Put(t.Context(), consulkv.EncodeKey(key), []byte(value))
		must.NoError(t, err)
	}
	put("app/config", "v1")
	put("app/db.url", "postgres://db")

	var saved structs.BackupSaveResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Save", &structs.BackupSaveRequest{
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &saved))
	record := saved.Backup
	must.NotNil(t, record)
	must.MapContainsKey(t, record.Destinations[0].Objects, structs.BackupObjectConsulKV)

	// The contents list the keys with the hash of their value, not the value.
	var contents structs.BackupContentsResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Contents", &structs.BackupContentsRequest{
		ID: record.ID, Type: structs.BackupContentConsulKV, AgeIdentity: identity.String(),
		QueryOptions: structs.QueryOptions{Region: "global"},
	}, &contents))
	must.Len(t, 2, contents.Contents.ConsulKV)
	must.Eq(t, "app/config", contents.Contents.ConsulKV[0].Key)
	must.Eq(t, 2, contents.Contents.ConsulKV[0].Size)
	must.Eq(t, "app/db.url", contents.Contents.ConsulKV[1].Key)

	// One key changes, one is deleted, one is added: the diff shows them.
	put("app/config", "v2")
	must.NoError(t, kv.Delete(t.Context(), consulkv.EncodeKey("app/db.url")))
	put("app/new", "x")
	var diff structs.BackupDiffResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Diff", &structs.BackupDiffRequest{
		ID: record.ID, Type: structs.BackupContentConsulKV, AgeIdentity: identity.String(),
		QueryOptions: structs.QueryOptions{Region: "global"},
	}, &diff))
	changes := map[string]string{}
	for _, d := range diff.Diff.Objects {
		changes[d.ID] = d.Change
	}
	must.Eq(t, map[string]string{
		"app/config": structs.BackupDiffModified,
		"app/db.url": structs.BackupDiffDeleted,
		"app/new":    structs.BackupDiffAdded,
	}, changes)

	restore := func(item *structs.BackupRestoreItem) *structs.BackupRestoreItemResult {
		var resp structs.BackupRestoreResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", &structs.BackupRestoreRequest{
			ID: record.ID, Items: []*structs.BackupRestoreItem{item}, AgeIdentity: identity.String(),
			WriteRequest: structs.WriteRequest{Region: "global"},
		}, &resp))
		must.Len(t, 1, resp.Results)
		return resp.Results[0]
	}

	// A deleted key is re-created.
	res := restore(&structs.BackupRestoreItem{Type: structs.BackupRestoreItemConsulKV, ID: "app/db.url"})
	must.Eq(t, "", res.Error)
	must.Eq(t, "create", res.Action)
	e, err := kv.Get(t.Context(), consulkv.EncodeKey("app/db.url"))
	must.NoError(t, err)
	must.Eq(t, "postgres://db", string(e.Value()))

	// An existing key needs its current revision as the check index.
	res = restore(&structs.BackupRestoreItem{Type: structs.BackupRestoreItemConsulKV, ID: "app/config"})
	must.StrContains(t, res.Error, "give that check index")
	current := res.CurrentIndex
	res = restore(&structs.BackupRestoreItem{Type: structs.BackupRestoreItemConsulKV, ID: "app/config",
		CheckIndex: pointer.Of(current)})
	must.Eq(t, "", res.Error)
	e, err = kv.Get(t.Context(), consulkv.EncodeKey("app/config"))
	must.NoError(t, err)
	must.Eq(t, "v1", string(e.Value()))

	// A key that is not in the backup is an error.
	res = restore(&structs.BackupRestoreItem{Type: structs.BackupRestoreItemConsulKV, ID: "app/none"})
	must.StrContains(t, res.Error, "not found in the backup")

	// The full restore replaces the bucket with the keys of the backup.
	export, err := srv.exportConsulKV(t.Context())
	must.NoError(t, err)
	must.NotNil(t, export.Entry("app/new"))
	must.NoError(t, srv.importConsulKV(t.Context(), []*consulkv.Entry{{Key: "app/config", Value: []byte("v1")}}, true))
	export, err = srv.exportConsulKV(t.Context())
	must.NoError(t, err)
	must.Len(t, 1, export.Entries)
	must.Eq(t, "app/config", export.Entries[0].Key)
}

// TestBusKV_Migrate checks the migration of the Consul KV from Variables to
// the bucket of the bus.
func TestBusKV_Migrate(t *testing.T) {
	ci.Parallel(t)

	srv, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)

	js := busJS(t, srv)
	var kv jetstream.KeyValue
	var err error
	waitBus(t, func() (bool, error) {
		kv, err = js.KeyValue(t.Context(), bus.ConsulKVBucket)
		return err == nil, err
	})

	// Keys as the variables backend of the clients keeps them.
	setVar := func(path string, items map[string]string) {
		var resp structs.VariablesApplyResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.VariablesApplyRPCMethod, &structs.VariablesApplyRequest{
			Op: structs.VarOpSet,
			Var: &structs.VariableDecrypted{
				VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: path},
				Items:            items,
			},
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
		}, &resp))
	}
	keyPath := func(key string) string { return consulkv.ACLPath(consulkv.DefaultPath, key) }
	setVar(keyPath("app/config"), consulkv.EncodeVariableItems([]byte("v1"), 0))
	setVar(keyPath("app/bin"), consulkv.EncodeVariableItems([]byte{0xff, 0x00}, 0))
	setVar(keyPath("app/same"), consulkv.EncodeVariableItems([]byte("s"), 0))
	setVar(keyPath("app/other"), consulkv.EncodeVariableItems([]byte("old"), 0))
	setVar(keyPath("app/flagged"), consulkv.EncodeVariableItems([]byte("f"), 7))
	setVar("nomad/jobs/unrelated", map[string]string{"a": "b"})

	// Two keys are already in the bucket.
	_, err = kv.Put(t.Context(), consulkv.EncodeKey("app/same"), []byte("s"))
	must.NoError(t, err)
	_, err = kv.Put(t.Context(), consulkv.EncodeKey("app/other"), []byte("new"))
	must.NoError(t, err)

	migrate := func(dryRun bool) *structs.ConsulKVMigrateResult {
		var resp structs.ConsulKVMigrateResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Operator.ConsulKVMigrate", &structs.ConsulKVMigrateRequest{
			DryRun:       dryRun,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
		}, &resp))
		must.NotNil(t, resp.Result)
		return resp.Result
	}

	// A dry run writes nothing.
	res := migrate(true)
	must.SliceContainsAll(t, []string{"app/config", "app/bin"}, res.Copied)
	must.Eq(t, []string{"app/same"}, res.Unchanged)
	must.Eq(t, []string{"app/other"}, res.Conflicts)
	must.Len(t, 1, res.Skipped)
	must.Eq(t, "app/flagged", res.Skipped[0].Key)
	_, err = kv.Get(t.Context(), consulkv.EncodeKey("app/config"))
	must.ErrorIs(t, err, jetstream.ErrKeyNotFound)

	res = migrate(false)
	must.SliceContainsAll(t, []string{"app/config", "app/bin"}, res.Copied)
	must.Eq(t, []string{"app/other"}, res.Conflicts)
	e, err := kv.Get(t.Context(), consulkv.EncodeKey("app/bin"))
	must.NoError(t, err)
	must.Eq(t, []byte{0xff, 0x00}, e.Value())
	e, err = kv.Get(t.Context(), consulkv.EncodeKey("app/other"))
	must.NoError(t, err)
	must.Eq(t, "new", string(e.Value()), must.Sprint("an existing key is not overwritten"))

	// A second run copies nothing more.
	res = migrate(false)
	must.Len(t, 0, res.Copied)
	must.SliceContainsAll(t, []string{"app/config", "app/bin", "app/same"}, res.Unchanged)
}

// TestBusKV_MigrateACL checks that a migration needs operator write and
// copies only the keys the token may read.
func TestBusKV_MigrateACL(t *testing.T) {
	ci.Parallel(t)

	srv, root, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)
	js := busJS(t, srv)
	waitBus(t, func() (bool, error) {
		_, err := js.KeyValue(t.Context(), bus.ConsulKVBucket)
		return err == nil, err
	})

	for _, key := range []string{"public/a", "secret/b"} {
		var resp structs.VariablesApplyResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.VariablesApplyRPCMethod, &structs.VariablesApplyRequest{
			Op: structs.VarOpSet,
			Var: &structs.VariableDecrypted{
				VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: consulkv.ACLPath(consulkv.DefaultPath, key)},
				Items:            consulkv.EncodeVariableItems([]byte("v"), 0),
			},
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: root.SecretID},
		}, &resp))
	}

	migrate := func(token string) (*structs.ConsulKVMigrateResult, error) {
		var resp structs.ConsulKVMigrateResponse
		err := msgpackrpc.CallWithCodec(codec, "Operator.ConsulKVMigrate", &structs.ConsulKVMigrateRequest{
			DryRun:       true,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token},
		}, &resp)
		return resp.Result, err
	}

	readOnly := mock.CreatePolicyAndToken(t, srv.State(), 1001, "operator-read", `operator { policy = "read" }`)
	_, err := migrate(readOnly.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	writer := mock.CreatePolicyAndToken(t, srv.State(), 1003, "operator-write-public", `
operator { policy = "write" }
namespace "default" {
  variables {
    path "consul/kv/public/*" { capabilities = ["read"] }
  }
}`)
	res, err := migrate(writer.SecretID)
	must.NoError(t, err)
	must.Eq(t, []string{"public/a"}, res.Copied)

	res, err = migrate(root.SecretID)
	must.NoError(t, err)
	must.Len(t, 2, res.Copied)
}

// TestBusKV_CarryOver checks that the Consul KV keys of a standalone bus
// are carried over when a second server makes the bus clustered.
func TestBusKV_CarryOver(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup1 := TestServer(t, func(c *Config) {
		enableBus(t)(c)
		c.NumSchedulers = 0
	})
	defer cleanup1()
	testutil.WaitForLeader(t, s1.RPC)
	must.False(t, s1.bus.Clustered())

	js := busJS(t, s1)
	var kv jetstream.KeyValue
	var err error
	waitBus(t, func() (bool, error) {
		kv, err = js.KeyValue(t.Context(), bus.ConsulKVBucket)
		return err == nil, err
	})
	_, err = kv.Put(t.Context(), consulkv.EncodeKey("app/config"), []byte("v1"))
	must.NoError(t, err)
	_, err = kv.Put(t.Context(), consulkv.EncodeKey("app/db.url"), []byte("postgres://db"))
	must.NoError(t, err)

	s2, cleanup2 := TestServer(t, func(c *Config) {
		enableBus(t)(c)
		c.BootstrapExpect = 0
		c.NumSchedulers = 0
	})
	defer cleanup2()
	TestJoin(t, s1, s2)

	// The bus of s1 restarts clustered; its keys come over from the store
	// kept aside, and the store is marked done.
	waitBus(t, func() (bool, error) {
		if !s1.bus.Clustered() {
			return false, errors.New("s1 is not clustered yet")
		}
		export, err := s2.exportConsulKV(t.Context())
		if err != nil {
			return false, err
		}
		if export == nil || len(export.Entries) != 2 {
			return false, fmt.Errorf("clustered bucket: %+v", export)
		}
		return true, nil
	})
	export, err := s2.exportConsulKV(t.Context())
	must.NoError(t, err)
	must.Eq(t, "postgres://db", string(export.Entry("app/db.url").Value))
	_, err = os.Stat(filepath.Join(s1.bus.StandaloneStoreDir(), busCarriedOverMarker))
	must.NoError(t, err)
}
