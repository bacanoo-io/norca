// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestFSM_SecretStoreBlobs(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	// Sent with the ignore flag, as the secret store backend does.
	buf, err := structs.Encode(structs.SecretStoreBlobsUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		structs.SecretStoreBlobsUpsertRequest{Blobs: []*structs.SecretStoreBlob{
			{Path: "core/keyring", Value: []byte("ciphertext-1")},
			{Path: "secret/data/app", Value: []byte("ciphertext-2")},
		}})
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	blob, err := fsm.State().SecretStoreBlobByPath(nil, "core/keyring")
	must.NoError(t, err)
	must.NotNil(t, blob)
	must.Eq(t, []byte("ciphertext-1"), blob.Value)
	must.Eq(t, 1, blob.ModifyIndex)

	buf, err = structs.Encode(structs.SecretStoreBlobsDeleteRequestType|structs.IgnoreUnknownTypeFlag,
		structs.SecretStoreBlobsDeleteRequest{Paths: []string{"secret/data/app", "missing"}})
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	blob, err = fsm.State().SecretStoreBlobByPath(nil, "secret/data/app")
	must.NoError(t, err)
	must.Nil(t, blob)
	blob, err = fsm.State().SecretStoreBlobByPath(nil, "core/keyring")
	must.NoError(t, err)
	must.NotNil(t, blob)
}

func TestFSM_SnapshotRestore_SecretStoreBlobs(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	blobs := []*structs.SecretStoreBlob{
		{Path: "core/keyring", Value: []byte("ciphertext-1")},
		{Path: "core/seal-config", Value: []byte(`{"type":"shamir","shares":5,"threshold":3}`)},
		{Path: "secret/data/app/db", Value: []byte{0, 1, 2, 255}},
	}
	must.NoError(t, fsm.State().UpsertSecretStoreBlobs(structs.MsgTypeTestSetup, 1000, blobs))

	fsm2 := testSnapshotRestore(t, fsm)
	for _, want := range blobs {
		got, err := fsm2.State().SecretStoreBlobByPath(nil, want.Path)
		must.NoError(t, err)
		must.Eq(t, want, got)
	}
	idx, err := fsm2.State().Index("secret_store_blobs")
	must.NoError(t, err)
	must.Eq(t, 1000, idx)
}
