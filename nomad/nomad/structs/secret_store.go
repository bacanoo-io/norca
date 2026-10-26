// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

// SecretStoreBlob is one entry of the physical storage of the built-in secret
// store (nomad/secretstore): the value the uBixVault barrier wrote at a
// storage path. The values are ciphertext, except for the barrier's
// non-secret bookkeeping (the seal configuration: seal type, number of shares
// and threshold); the servers replicate and snapshot them as opaque bytes and
// never decrypt them. They are not published on the event stream.
type SecretStoreBlob struct {
	// Path is the "/"-separated storage path, for example "core/keyring".
	Path string

	// Value is the stored bytes.
	Value []byte

	CreateIndex uint64
	ModifyIndex uint64
}

// SecretStoreBlobsUpsertRequest writes secret store blobs. It is applied
// through Raft by the leader only; there is no RPC endpoint for it.
type SecretStoreBlobsUpsertRequest struct {
	Blobs []*SecretStoreBlob
	WriteRequest
}

// SecretStoreBlobsDeleteRequest deletes secret store blobs by path. Paths
// that do not exist are ignored. It is applied through Raft by the leader
// only; there is no RPC endpoint for it.
type SecretStoreBlobsDeleteRequest struct {
	Paths []string
	WriteRequest
}
