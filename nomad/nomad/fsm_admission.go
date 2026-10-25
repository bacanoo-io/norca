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

func (n *nomadFSM) applyAdmissionPolicyUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_admission_policy_upsert"}, time.Now())
	var req structs.AdmissionPolicyUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertAdmissionPolicies(msgType, index, req.AdmissionPolicies); err != nil {
		n.logger.Error("UpsertAdmissionPolicies failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyAdmissionPolicyDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_admission_policy_delete"}, time.Now())
	var req structs.AdmissionPolicyDeleteRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteAdmissionPolicies(msgType, index, req.Names); err != nil {
		n.logger.Error("DeleteAdmissionPolicies failed", "error", err)
		return err
	}
	return nil
}

func (s *nomadSnapshot) persistAdmissionPolicies(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	policies, err := s.snap.AdmissionPolicies(ws, state.SortDefault)
	if err != nil {
		return err
	}

	for raw := policies.Next(); raw != nil; raw = policies.Next() {
		policy := raw.(*structs.AdmissionPolicy)

		sink.Write([]byte{byte(AdmissionPolicySnapshot)})
		if err := encoder.Encode(policy); err != nil {
			return err
		}
	}
	return nil
}
