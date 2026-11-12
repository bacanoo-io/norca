// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/raft"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// AutopilotConfigSnapshot is the snapshot type of the autopilot
// configuration (clean room: Norca's own addition).
//
// The autopilot configuration was written to the Raft log only, never to a
// snapshot. A server that restored its state from a snapshot taken after the
// last change of the configuration had none; when such a server became the
// leader it made a new one from its own agent configuration
// (getOrCreateAutopilotConfig) and wrote it for the whole region. What had
// been set with `operator autopilot set-config` was gone: with standby
// servers that is target_voters, and with it the failover.
const AutopilotConfigSnapshot SnapshotType = 116

// persistAutopilotConfig persists the autopilot configuration.
func (s *nomadSnapshot) persistAutopilotConfig(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	_, cfg, err := s.snap.AutopilotConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	sink.Write([]byte{byte(AutopilotConfigSnapshot)})
	return encoder.Encode(cfg)
}

// restoreAutopilotConfigSnapshot restores the autopilot configuration.
func restoreAutopilotConfigSnapshot(restore *state.StateRestore, dec *codec.Decoder) error {
	cfg := new(structs.AutopilotConfig)
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	return restore.AutopilotConfigRestore(cfg)
}
