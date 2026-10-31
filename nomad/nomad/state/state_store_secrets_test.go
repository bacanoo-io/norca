// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func testSecretRow(ns, path string) *structs.Secret {
	return &structs.Secret{
		Namespace:      ns,
		Path:           path,
		CurrentVersion: 1,
		ValueHash:      structs.SecretValueHashPrefix + "00",
		Versions:       []*structs.SecretVersion{{Version: 1, ValueHash: structs.SecretValueHashPrefix + "00"}},
	}
}

func TestStateStore_Secrets(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)

	// Create a row with its blobs.
	ws := memdb.NewWatchSet()
	_, err := s.SecretByPath(ws, "default", "a/b")
	must.NoError(t, err)
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 10, &structs.SecretApplyRequest{
		Secret:      testSecretRow("default", "a/b"),
		Blobs:       []*structs.SecretStoreBlob{{Path: "secrets/meta/default/a/b", Value: []byte("m")}, {Path: "secrets/data/default/a/b/1", Value: []byte("d")}},
		CreateBlobs: []string{"key"},
	}))
	must.True(t, watchFired(ws))
	row, err := s.SecretByPath(nil, "default", "a/b")
	must.NoError(t, err)
	must.Eq(t, 10, row.CreateIndex)
	must.Eq(t, 10, row.ModifyIndex)
	blob, err := s.SecretStoreBlobByPath(nil, "secrets/data/default/a/b/1")
	must.NoError(t, err)
	must.Eq(t, []byte("d"), blob.Value)
	index, err := s.Index(TableSecrets)
	must.NoError(t, err)
	must.Eq(t, 10, index)

	// A stale expected index is a conflict and changes nothing.
	err = s.ApplySecret(structs.MsgTypeTestSetup, 11, &structs.SecretApplyRequest{
		Secret:        testSecretRow("default", "a/b"),
		ExpectedIndex: 9,
		DeleteBlobs:   []string{"secrets/data/default/a/b/1"},
	})
	must.ErrorIs(t, err, structs.ErrSecretConflict)
	blob, err = s.SecretStoreBlobByPath(nil, "secrets/data/default/a/b/1")
	must.NoError(t, err)
	must.NotNil(t, blob)

	// So is the creation of an existing blob.
	must.NoError(t, s.UpsertSecretStoreBlobs(structs.MsgTypeTestSetup, 12, []*structs.SecretStoreBlob{{Path: "key", Value: []byte("k")}}))
	err = s.ApplySecret(structs.MsgTypeTestSetup, 13, &structs.SecretApplyRequest{
		Secret: testSecretRow("default", "a/b"), ExpectedIndex: 10, CreateBlobs: []string{"key"},
	})
	must.ErrorIs(t, err, structs.ErrSecretConflict)

	// An update keeps the create index and deletes blobs.
	update := testSecretRow("default", "a/b")
	update.CurrentVersion = 2
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 14, &structs.SecretApplyRequest{
		Secret: update, ExpectedIndex: 10, DeleteBlobs: []string{"secrets/data/default/a/b/1"},
	}))
	row, err = s.SecretByPath(nil, "default", "a/b")
	must.NoError(t, err)
	must.Eq(t, 10, row.CreateIndex)
	must.Eq(t, 14, row.ModifyIndex)
	blob, err = s.SecretStoreBlobByPath(nil, "secrets/data/default/a/b/1")
	must.NoError(t, err)
	must.Nil(t, blob)

	// Listing by namespace and prefix.
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 15, &structs.SecretApplyRequest{Secret: testSecretRow("default", "a/c")}))
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 16, &structs.SecretApplyRequest{Secret: testSecretRow("other", "a/d")}))
	iter, err := s.SecretsByNamespacePrefix(nil, "default", "a/")
	must.NoError(t, err)
	var paths []string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		paths = append(paths, raw.(*structs.Secret).Path)
	}
	must.Eq(t, []string{"a/b", "a/c"}, paths)
	iter, err = s.Secrets(nil)
	must.NoError(t, err)
	n := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		n++
	}
	must.Eq(t, 3, n)

	// Purge.
	err = s.PurgeSecret(structs.MsgTypeTestSetup, 17, &structs.SecretPurgeRequest{Namespace: "default", Path: "a/b", ExpectedIndex: 1})
	must.ErrorIs(t, err, structs.ErrSecretConflict)
	must.NoError(t, s.PurgeSecret(structs.MsgTypeTestSetup, 18, &structs.SecretPurgeRequest{
		Namespace: "default", Path: "a/b", ExpectedIndex: 14, DeleteBlobs: []string{"secrets/meta/default/a/b"},
	}))
	row, err = s.SecretByPath(nil, "default", "a/b")
	must.NoError(t, err)
	must.Nil(t, row)
	blob, err = s.SecretStoreBlobByPath(nil, "secrets/meta/default/a/b")
	must.NoError(t, err)
	must.Nil(t, blob)
}

func TestStateStore_SecretEvents(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	req := &structs.SecretApplyRequest{
		Secret: testSecretRow("default", "ev"),
		Blobs:  []*structs.SecretStoreBlob{{Path: "secrets/data/default/ev/1", Value: []byte("ciphertext")}},
	}
	must.NoError(t, s.ApplySecret(structs.SecretApplyRequestType, 100, req))

	events := WaitForEvents(t, s, 100, 1, 2e9)
	must.Len(t, 1, events)
	must.Eq(t, structs.TopicSecret, events[0].Topic)
	must.Eq(t, structs.TypeSecretUpserted, events[0].Type)
	must.Eq(t, "ev", events[0].Key)
	payload := events[0].Payload.(*structs.SecretEvent)
	must.Eq(t, "ev", payload.Secret.Path)

	must.NoError(t, s.PurgeSecret(structs.SecretPurgeRequestType, 101, &structs.SecretPurgeRequest{
		Namespace: "default", Path: "ev", ExpectedIndex: 100,
	}))
	events = WaitForEvents(t, s, 101, 1, 2e9)
	must.Len(t, 1, events)
	must.Eq(t, structs.TypeSecretDeleted, events[0].Type)
}

func TestStateStore_SecretReplicas(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)

	replica := func(createIndex, version uint64) *structs.Secret {
		row := testSecretRow("default", "r")
		row.CurrentVersion = version
		row.Replica = &structs.SecretReplica{Region: "region1", SourceCreateIndex: createIndex, SourceModifyIndex: 100 + version}
		return row
	}

	// A replica is created and updated to a newer version.
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 10, &structs.SecretApplyRequest{Secret: replica(5, 2)}))
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 11, &structs.SecretApplyRequest{Secret: replica(5, 3), ExpectedIndex: 10}))

	// The same state again is idempotent.
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 12, &structs.SecretApplyRequest{Secret: replica(5, 3), ExpectedIndex: 11}))

	// An older version never overwrites a newer one.
	err := s.ApplySecret(structs.MsgTypeTestSetup, 13, &structs.SecretApplyRequest{Secret: replica(5, 2), ExpectedIndex: 12})
	must.ErrorIs(t, err, structs.ErrSecretReplicaConflict)
	row, err := s.SecretByPath(nil, "default", "r")
	must.NoError(t, err)
	must.Eq(t, 3, row.CurrentVersion)
	must.Eq(t, 12, row.ModifyIndex)

	// The secret recreated in the authoritative region starts at version 1.
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 14, &structs.SecretApplyRequest{Secret: replica(50, 1), ExpectedIndex: 12}))
	row, err = s.SecretByPath(nil, "default", "r")
	must.NoError(t, err)
	must.Eq(t, 1, row.CurrentVersion)
	must.Eq(t, 50, row.Replica.SourceCreateIndex)

	// A replica never overwrites a secret of the region itself.
	must.NoError(t, s.ApplySecret(structs.MsgTypeTestSetup, 15, &structs.SecretApplyRequest{Secret: testSecretRow("default", "local")}))
	local := replica(5, 9)
	local.Path = "local"
	err = s.ApplySecret(structs.MsgTypeTestSetup, 16, &structs.SecretApplyRequest{Secret: local, ExpectedIndex: 15})
	must.ErrorIs(t, err, structs.ErrSecretReplicaConflict)
	row, err = s.SecretByPath(nil, "default", "local")
	must.NoError(t, err)
	must.Nil(t, row.Replica)

	// The tombstone of a replica removes it.
	must.NoError(t, s.PurgeSecret(structs.MsgTypeTestSetup, 17, &structs.SecretPurgeRequest{
		Namespace: "default", Path: "r", ExpectedIndex: 14,
	}))
	row, err = s.SecretByPath(nil, "default", "r")
	must.NoError(t, err)
	must.Nil(t, row)
}
