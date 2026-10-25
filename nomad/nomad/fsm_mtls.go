// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/raft"
)

// Snapshot types of the native mTLS tables. They use the numbers of the Raft
// message types assigned to native mTLS.
const (
	MTLSRootSnapshot         SnapshotType = 97
	ServiceIntentionSnapshot SnapshotType = 98
)

func (n *nomadFSM) applyMTLSRootsUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_mtls_roots_upsert"}, time.Now())
	var req structs.MTLSRootsUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertMTLSRoots(msgType, index, req.Roots, req.DeleteIDs); err != nil {
		n.logger.Error("UpsertMTLSRoots failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyServiceIntentionUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_service_intention_upsert"}, time.Now())
	var req structs.ServiceIntentionUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertServiceIntentions(msgType, index, req.Intentions); err != nil {
		n.logger.Error("UpsertServiceIntentions failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyServiceIntentionDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_service_intention_delete"}, time.Now())
	var req structs.ServiceIntentionDeleteRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteServiceIntentions(msgType, index, req.Intentions); err != nil {
		n.logger.Error("DeleteServiceIntentions failed", "error", err)
		return err
	}
	return nil
}

// restoreMTLSSnapshot restores a trust bundle root or a service intention.
func restoreMTLSSnapshot(snapType SnapshotType, restore *state.StateRestore, dec *codec.Decoder) error {
	switch snapType {
	case MTLSRootSnapshot:
		root := new(structs.MTLSRoot)
		if err := dec.Decode(root); err != nil {
			return err
		}
		return restore.MTLSRootRestore(root)
	case ServiceIntentionSnapshot:
		intention := new(structs.ServiceIntention)
		if err := dec.Decode(intention); err != nil {
			return err
		}
		return restore.ServiceIntentionRestore(intention)
	}
	return fmt.Errorf("unexpected mtls snapshot type %d", snapType)
}

// persistMTLS persists the trust bundle roots, with the encrypted CA keys,
// and the service intentions. The roots are part of every Raft snapshot and
// therefore of every backup; the root keys that decrypt the CA keys are in
// the keyring object of the backup.
func (s *nomadSnapshot) persistMTLS(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	roots, err := s.snap.MTLSRoots(ws)
	if err != nil {
		return err
	}
	for _, root := range roots {
		sink.Write([]byte{byte(MTLSRootSnapshot)})
		if err := encoder.Encode(root); err != nil {
			return err
		}
	}

	intentions, err := s.snap.ServiceIntentions(ws, structs.AllNamespacesSentinel)
	if err != nil {
		return err
	}
	for _, intention := range intentions {
		sink.Write([]byte{byte(ServiceIntentionSnapshot)})
		if err := encoder.Encode(intention); err != nil {
			return err
		}
	}
	return nil
}
