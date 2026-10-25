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

// Snapshot types of the network policy tables. They use the numbers of the
// Raft upsert message types assigned to network policies.
const (
	NetworkPolicySnapshot      SnapshotType = 101
	AdminNetworkPolicySnapshot SnapshotType = 103
)

func (n *nomadFSM) applyNetworkPolicyUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_network_policy_upsert"}, time.Now())
	var req structs.NetworkPolicyUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertNetworkPolicies(msgType, index, req.NetworkPolicies); err != nil {
		n.logger.Error("UpsertNetworkPolicies failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyNetworkPolicyDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_network_policy_delete"}, time.Now())
	var req structs.NetworkPolicyDeleteRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteNetworkPolicies(msgType, index, req.RequestNamespace(), req.Names); err != nil {
		n.logger.Error("DeleteNetworkPolicies failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyAdminNetworkPolicyUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_admin_network_policy_upsert"}, time.Now())
	var req structs.AdminNetworkPolicyUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertAdminNetworkPolicies(msgType, index, req.AdminNetworkPolicies); err != nil {
		n.logger.Error("UpsertAdminNetworkPolicies failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyAdminNetworkPolicyDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_admin_network_policy_delete"}, time.Now())
	var req structs.AdminNetworkPolicyDeleteRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteAdminNetworkPolicies(msgType, index, req.Names); err != nil {
		n.logger.Error("DeleteAdminNetworkPolicies failed", "error", err)
		return err
	}
	return nil
}

// restoreNetworkPolicySnapshot restores a network policy or an admin network
// policy.
func restoreNetworkPolicySnapshot(snapType SnapshotType, restore *state.StateRestore, dec *codec.Decoder) error {
	switch snapType {
	case NetworkPolicySnapshot:
		policy := new(structs.NetworkPolicy)
		if err := dec.Decode(policy); err != nil {
			return err
		}
		return restore.NetworkPolicyRestore(policy)
	case AdminNetworkPolicySnapshot:
		policy := new(structs.AdminNetworkPolicy)
		if err := dec.Decode(policy); err != nil {
			return err
		}
		return restore.AdminNetworkPolicyRestore(policy)
	}
	return fmt.Errorf("unexpected network policy snapshot type %d", snapType)
}

// persistNetworkPolicies persists the network policies and the admin
// network policies.
func (s *nomadSnapshot) persistNetworkPolicies(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	policies, err := s.snap.NetworkPolicies(ws, state.SortDefault)
	if err != nil {
		return err
	}
	for raw := policies.Next(); raw != nil; raw = policies.Next() {
		policy := raw.(*structs.NetworkPolicy)
		sink.Write([]byte{byte(NetworkPolicySnapshot)})
		if err := encoder.Encode(policy); err != nil {
			return err
		}
	}

	admin, err := s.snap.AdminNetworkPolicies(ws, state.SortDefault)
	if err != nil {
		return err
	}
	for raw := admin.Next(); raw != nil; raw = admin.Next() {
		policy := raw.(*structs.AdminNetworkPolicy)
		sink.Write([]byte{byte(AdminNetworkPolicySnapshot)})
		if err := encoder.Encode(policy); err != nil {
			return err
		}
	}
	return nil
}
