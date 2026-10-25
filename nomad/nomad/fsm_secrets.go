// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"errors"
	"fmt"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/raft"
)

// SecretSnapshot is the snapshot type of the secrets table. It uses the
// number of the Raft apply message type assigned to the secret store.
const SecretSnapshot SnapshotType = 109

func (n *nomadFSM) applySecretApply(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_secret"}, time.Now())
	var req structs.SecretApplyRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.ApplySecret(msgType, index, &req); err != nil {
		if !errors.Is(err, structs.ErrSecretConflict) {
			n.logger.Error("ApplySecret failed", "error", err)
		}
		return err
	}
	return nil
}

func (n *nomadFSM) applySecretPurge(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "purge_secret"}, time.Now())
	var req structs.SecretPurgeRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.PurgeSecret(msgType, index, &req); err != nil {
		if !errors.Is(err, structs.ErrSecretConflict) {
			n.logger.Error("PurgeSecret failed", "error", err)
		}
		return err
	}
	return nil
}

// restoreSecretSnapshot restores a secret's metadata row.
func restoreSecretSnapshot(restore *state.StateRestore, dec *codec.Decoder) error {
	secret := new(structs.Secret)
	if err := dec.Decode(secret); err != nil {
		return err
	}
	return restore.SecretRestore(secret)
}

// persistSecrets persists the metadata rows of the secrets; their values are
// persisted as ciphertext with the secret store blobs.
func (s *nomadSnapshot) persistSecrets(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	secrets, err := s.snap.Secrets(ws)
	if err != nil {
		return err
	}
	for raw := secrets.Next(); raw != nil; raw = secrets.Next() {
		secret := raw.(*structs.Secret)
		sink.Write([]byte{byte(SecretSnapshot)})
		if err := encoder.Encode(secret); err != nil {
			return err
		}
	}
	return nil
}
