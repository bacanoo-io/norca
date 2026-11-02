// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/raft"
)

// The snapshot types of the engines and the leases of the dynamic secrets.
// They use the numbers of the Raft apply message types of their tables.
const (
	SecretEngineSnapshot SnapshotType = 112
	SecretLeaseSnapshot  SnapshotType = 114
)

// isSecretEngineApplyError reports the expected errors of the engine and
// lease applies, which the leader handles and which are not logged.
func isSecretEngineApplyError(err error) bool {
	return errors.Is(err, structs.ErrSecretConflict) || errors.Is(err, structs.ErrSecretEngineNotFound) ||
		errors.Is(err, structs.ErrSecretLeaseRevoked)
}

func (n *nomadFSM) applySecretEngineApply(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_secret_engine"}, time.Now())
	var req structs.SecretEngineApplyRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}
	if err := n.state.ApplySecretEngine(msgType, index, &req); err != nil {
		if !isSecretEngineApplyError(err) {
			n.logger.Error("ApplySecretEngine failed", "error", err)
		}
		return err
	}
	return nil
}

func (n *nomadFSM) applySecretEnginePurge(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "purge_secret_engine"}, time.Now())
	var req structs.SecretEnginePurgeRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}
	if err := n.state.PurgeSecretEngine(msgType, index, &req); err != nil {
		if !isSecretEngineApplyError(err) {
			n.logger.Error("PurgeSecretEngine failed", "error", err)
		}
		return err
	}
	return nil
}

func (n *nomadFSM) applySecretLeaseApply(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_secret_lease"}, time.Now())
	var req structs.SecretLeaseApplyRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}
	if err := n.state.ApplySecretLeases(msgType, index, &req); err != nil {
		if !isSecretEngineApplyError(err) {
			n.logger.Error("ApplySecretLeases failed", "error", err)
		}
		return err
	}
	return nil
}

func (n *nomadFSM) applySecretLeasePurge(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "purge_secret_lease"}, time.Now())
	var req structs.SecretLeasePurgeRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}
	if err := n.state.PurgeSecretLeases(msgType, index, &req); err != nil {
		n.logger.Error("PurgeSecretLeases failed", "error", err)
		return err
	}
	return nil
}

// restoreSecretEngineSnapshot restores an engine or a lease.
func restoreSecretEngineSnapshot(snapType SnapshotType, restore *state.StateRestore, dec *codec.Decoder) error {
	switch snapType {
	case SecretEngineSnapshot:
		engine := new(structs.SecretEngine)
		if err := dec.Decode(engine); err != nil {
			return err
		}
		return restore.SecretEngineRestore(engine)
	case SecretLeaseSnapshot:
		lease := new(structs.SecretLease)
		if err := dec.Decode(lease); err != nil {
			return err
		}
		return restore.SecretLeaseRestore(lease)
	}
	return fmt.Errorf("unknown secret engine snapshot type %d", snapType)
}

// persistSecretEngines persists the engines and the leases.
func (s *nomadSnapshot) persistSecretEngines(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	engines, err := s.snap.SecretEngines(ws)
	if err != nil {
		return err
	}
	for raw := engines.Next(); raw != nil; raw = engines.Next() {
		sink.Write([]byte{byte(SecretEngineSnapshot)})
		if err := encoder.Encode(raw.(*structs.SecretEngine)); err != nil {
			return err
		}
	}
	leases, err := s.snap.SecretLeases(ws)
	if err != nil {
		return err
	}
	for raw := leases.Next(); raw != nil; raw = leases.Next() {
		sink.Write([]byte{byte(SecretLeaseSnapshot)})
		if err := encoder.Encode(raw.(*structs.SecretLease)); err != nil {
			return err
		}
	}
	return nil
}
