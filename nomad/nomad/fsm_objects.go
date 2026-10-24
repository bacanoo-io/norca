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

// Norca operator framework snapshot types mirror their message types.
const (
	ObjectKindSnapshot   SnapshotType = 82
	CustomObjectSnapshot SnapshotType = 83
)

func (n *nomadFSM) applyObjectKindUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_object_kind_upsert"}, time.Now())
	var req structs.ObjectKindUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertObjectKind(msgType, index, req.ObjectKind); err != nil {
		n.logger.Error("UpsertObjectKind failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyObjectKindDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_object_kind_delete"}, time.Now())
	var req structs.ObjectKindDeleteRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteObjectKind(msgType, index, req.Group, req.Kind); err != nil {
		n.logger.Error("DeleteObjectKind failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyCustomObjectUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_custom_object_upsert"}, time.Now())
	var req structs.CustomObjectUpsertStateRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	// A check-and-set conflict is an expected outcome, not an FSM failure.
	if err := n.state.UpsertCustomObject(msgType, index, req.Object, req.ExpectedModifyIndex); err != nil {
		if !structs.IsObjectConflictError(err) {
			n.logger.Error("UpsertCustomObject failed", "error", err)
		}
		return err
	}
	return nil
}

func (n *nomadFSM) applyCustomObjectDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_custom_object_delete"}, time.Now())
	var req structs.CustomObjectDeleteStateRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteCustomObject(msgType, index, req.Group, req.Kind, req.Namespace, req.Name, req.ExpectedModifyIndex); err != nil {
		if !structs.IsObjectConflictError(err) {
			n.logger.Error("DeleteCustomObject failed", "error", err)
		}
		return err
	}
	return nil
}

// restoreObjectSnapshot restores an object kind or a custom object.
func restoreObjectSnapshot(snapType SnapshotType, restore *state.StateRestore, dec *codec.Decoder) error {
	switch snapType {
	case ObjectKindSnapshot:
		kind := new(structs.ObjectKind)
		if err := dec.Decode(kind); err != nil {
			return err
		}
		return restore.ObjectKindRestore(kind)
	case CustomObjectSnapshot:
		obj := new(structs.CustomObject)
		if err := dec.Decode(obj); err != nil {
			return err
		}
		return restore.CustomObjectRestore(obj)
	default:
		return fmt.Errorf("unexpected object snapshot type %d", snapType)
	}
}

// persistObjects persists the object kinds and the custom objects. Kinds are
// written first so a restore never sees an object before its kind.
func (s *nomadSnapshot) persistObjects(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	kinds, err := s.snap.ObjectKinds(ws, "", state.SortDefault)
	if err != nil {
		return err
	}

	var refs []*structs.ObjectKind
	for raw := kinds.Next(); raw != nil; raw = kinds.Next() {
		kind := raw.(*structs.ObjectKind)
		refs = append(refs, kind)

		sink.Write([]byte{byte(ObjectKindSnapshot)})
		if err := encoder.Encode(kind); err != nil {
			return err
		}
	}

	for _, kind := range refs {
		objects, err := s.snap.CustomObjects(ws, kind.Group, kind.Kind, structs.AllNamespacesSentinel, state.SortDefault)
		if err != nil {
			return err
		}
		for raw := objects.Next(); raw != nil; raw = objects.Next() {
			obj := raw.(*structs.CustomObject)

			sink.Write([]byte{byte(CustomObjectSnapshot)})
			if err := encoder.Encode(obj); err != nil {
				return err
			}
		}
	}
	return nil
}
