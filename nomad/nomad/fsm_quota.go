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

// Snapshot types of the quota tables. They use the same numbers as the Raft
// message types assigned to quotas to avoid collisions between features
// developed in parallel.
const (
	QuotaSpecSnapshot  SnapshotType = 66
	QuotaUsageSnapshot SnapshotType = 67
)

// applyQuotaSpecUpsert is used to upsert a set of quota specifications.
func (n *nomadFSM) applyQuotaSpecUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_quota_spec_upsert"}, time.Now())
	var req structs.QuotaSpecUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertQuotaSpecs(msgType, index, req.Quotas); err != nil {
		n.logger.Error("UpsertQuotaSpecs failed", "error", err)
		return err
	}

	// A changed quota may allow blocked evaluations to make progress.
	for _, spec := range req.Quotas {
		n.blockedEvals.UnblockQuota(spec.Name, index)
	}
	return nil
}

// applyQuotaSpecDelete is used to delete a set of quota specifications.
func (n *nomadFSM) applyQuotaSpecDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_quota_spec_delete"}, time.Now())
	var req structs.QuotaSpecDeleteRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteQuotaSpecs(msgType, index, req.Names); err != nil {
		n.logger.Error("DeleteQuotaSpecs failed", "error", err)
		return err
	}
	return nil
}

// allocQuota returns the quota object associated with the allocation, or
// an empty string if the allocation's namespace has no quota.
func (n *nomadFSM) allocQuota(allocID string) (string, error) {
	alloc, err := n.state.AllocByID(nil, allocID)
	if err != nil {
		return "", err
	}
	if alloc == nil {
		return "", nil
	}

	ns, err := n.state.NamespaceByName(nil, alloc.Namespace)
	if err != nil {
		return "", err
	}
	if ns == nil {
		return "", nil
	}
	return ns.Quota, nil
}

// unblockQuotasForPlanResults unblocks the evaluations waiting on the quotas
// of allocations stopped or preempted by a plan. Usage is released as soon as
// the plan marks an allocation as terminal, which is not always followed by
// a client update (for example for allocations on lost nodes).
func (n *nomadFSM) unblockQuotasForPlanResults(req *structs.ApplyPlanResultsRequest, index uint64) {
	if !n.blockedEvals.Enabled() {
		return
	}

	ids := make([]string, 0, len(req.AllocsStopped)+len(req.AllocsPreempted)+len(req.NodePreemptions))
	for _, diff := range req.AllocsStopped {
		ids = append(ids, diff.ID)
	}
	for _, diff := range req.AllocsPreempted {
		ids = append(ids, diff.ID)
	}
	for _, alloc := range req.NodePreemptions {
		ids = append(ids, alloc.ID)
	}
	for _, alloc := range req.Alloc {
		if alloc.TerminalStatus() {
			ids = append(ids, alloc.ID)
		}
	}

	quotas := make(map[string]struct{})
	for _, id := range ids {
		quota, err := n.allocQuota(id)
		if err != nil {
			n.logger.Error("looking up quota associated with alloc failed", "alloc_id", id, "error", err)
			continue
		}
		if quota != "" {
			quotas[quota] = struct{}{}
		}
	}
	for quota := range quotas {
		n.blockedEvals.UnblockQuota(quota, index)
	}
}

// restoreQuotaSnapshot restores a quota specification or usage record.
func restoreQuotaSnapshot(snapType SnapshotType, restore *state.StateRestore, dec *codec.Decoder) error {
	switch snapType {
	case QuotaSpecSnapshot:
		spec := new(structs.QuotaSpec)
		if err := dec.Decode(spec); err != nil {
			return err
		}
		return restore.QuotaSpecRestore(spec)
	case QuotaUsageSnapshot:
		usage := new(structs.QuotaUsage)
		if err := dec.Decode(usage); err != nil {
			return err
		}
		return restore.QuotaUsageRestore(usage)
	default:
		return fmt.Errorf("unrecognized quota snapshot type: %v", snapType)
	}
}

// persistQuotas persists the quota specifications and usages.
func (s *nomadSnapshot) persistQuotas(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	specs, err := s.snap.QuotaSpecs(ws, state.SortDefault)
	if err != nil {
		return err
	}
	for raw := specs.Next(); raw != nil; raw = specs.Next() {
		spec := raw.(*structs.QuotaSpec)
		sink.Write([]byte{byte(QuotaSpecSnapshot)})
		if err := encoder.Encode(spec); err != nil {
			return err
		}
	}

	usages, err := s.snap.QuotaUsages(ws, state.SortDefault)
	if err != nil {
		return err
	}
	for raw := usages.Next(); raw != nil; raw = usages.Next() {
		usage := raw.(*structs.QuotaUsage)
		sink.Write([]byte{byte(QuotaUsageSnapshot)})
		if err := encoder.Encode(usage); err != nil {
			return err
		}
	}
	return nil
}
