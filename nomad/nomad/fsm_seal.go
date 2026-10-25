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

// SealConfigSnapshot is the snapshot type of the seal configuration. It uses
// the number of the Raft upsert message type assigned to the seal.
const SealConfigSnapshot SnapshotType = 107

func (n *nomadFSM) applySealConfigUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_seal_config_upsert"}, time.Now())
	var req structs.SealConfigUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertSealConfig(msgType, index, req.Config); err != nil {
		n.logger.Error("UpsertSealConfig failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applySealRewrapDone(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_seal_rewrap_done"}, time.Now())
	var req structs.SealRewrapDoneRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.SealRewrapReport(msgType, index, req.RewrapID, req.ServerID, req.Sealed); err != nil {
		n.logger.Error("SealRewrapDone failed", "error", err)
		return err
	}
	return nil
}

// restoreSealConfigSnapshot restores the seal configuration.
func restoreSealConfigSnapshot(restore *state.StateRestore, dec *codec.Decoder) error {
	cfg := new(structs.SealConfig)
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	return restore.SealConfigRestore(cfg)
}

// persistSealConfig persists the seal configuration.
func (s *nomadSnapshot) persistSealConfig(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	cfg, err := s.snap.SealConfig(memdb.NewWatchSet())
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	sink.Write([]byte{byte(SealConfigSnapshot)})
	return encoder.Encode(cfg)
}
