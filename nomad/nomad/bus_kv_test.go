// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	"filippo.io/age"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/helper/pointer"
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
