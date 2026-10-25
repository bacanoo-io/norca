// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/raft"
)

// SecretStoreBlobSnapshot is the snapshot type of the secret store blobs
// table. It uses the number of the Raft upsert message type assigned to the
// secret store.
const SecretStoreBlobSnapshot SnapshotType = 105

func (n *nomadFSM) applySecretStoreBlobsUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_secret_store_blobs_upsert"}, time.Now())
	var req structs.SecretStoreBlobsUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertSecretStoreBlobs(msgType, index, req.Blobs); err != nil {
		n.logger.Error("UpsertSecretStoreBlobs failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applySecretStoreBlobsDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_secret_store_blobs_delete"}, time.Now())
	var req structs.SecretStoreBlobsDeleteRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteSecretStoreBlobs(msgType, index, req.Paths); err != nil {
		n.logger.Error("DeleteSecretStoreBlobs failed", "error", err)
		return err
	}
	return nil
}

// restoreSecretStoreBlobSnapshot restores a secret store blob.
func restoreSecretStoreBlobSnapshot(restore *state.StateRestore, dec *codec.Decoder) error {
	blob := new(structs.SecretStoreBlob)
	if err := dec.Decode(blob); err != nil {
		return err
	}
	return restore.SecretStoreBlobRestore(blob)
}

// persistSecretStoreBlobs persists the secret store blobs as they are stored:
// ciphertext, so snapshots and backups never contain secret values in the
// clear. Restoring them still requires unsealing the secret store.
func (s *nomadSnapshot) persistSecretStoreBlobs(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	blobs, err := s.snap.SecretStoreBlobsByPrefix(ws, "")
	if err != nil {
		return err
	}
	for raw := blobs.Next(); raw != nil; raw = blobs.Next() {
		blob := raw.(*structs.SecretStoreBlob)
		sink.Write([]byte{byte(SecretStoreBlobSnapshot)})
		if err := encoder.Encode(blob); err != nil {
			return err
		}
	}
	return nil
}
