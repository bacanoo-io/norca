// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
)

// EvictAlloc marks an allocation for migration and creates the eviction's
// evaluation if the disruption budget of the allocation's task group allows
// it, otherwise it returns an error wrapping
// structs.ErrDisruptionBudgetExceeded. The budget is checked in the same
// transaction as the write, so concurrent evictions cannot together exceed
// it.
//
// It returns false without changing anything if the allocation is already
// terminal or already marked for migration.
func (s *StateStore) EvictAlloc(msgType structs.MessageType, index uint64, req *structs.AllocEvictRequest) (bool, error) {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	raw, err := txn.First("allocs", "id", req.AllocID)
	if err != nil {
		return false, fmt.Errorf("alloc lookup failed: %v", err)
	}
	if raw == nil {
		return false, structs.NewErrUnknownAllocation(req.AllocID)
	}
	alloc := raw.(*structs.Allocation)
	if alloc.TerminalStatus() || alloc.DesiredTransition.ShouldMigrate() {
		return false, nil
	}

	status, err := s.allocDisruptionStatusTxn(txn, alloc)
	if err != nil {
		return false, err
	}
	if status != nil && len(status.Disruptable([]*structs.Allocation{alloc})) == 0 {
		return false, status.Error()
	}

	transition := &structs.DesiredTransition{
		Migrate:         pointer.Of(true),
		NoShutdownDelay: pointer.Of(req.NoShutdownDelay),
	}
	if err := s.UpdateAllocDesiredTransitionTxn(txn, index, req.AllocID, transition); err != nil {
		return false, err
	}
	if req.Eval != nil {
		if err := s.nestedUpsertEval(txn, index, req.Eval); err != nil {
			return false, err
		}
	}
	if err := txn.Insert("index", &IndexEntry{"allocs", index}); err != nil {
		return false, fmt.Errorf("index update failed: %v", err)
	}
	return true, txn.Commit()
}

// allocDisruptionStatusTxn returns the disruption budget status of the task
// group of the allocation, or nil if the current version of its job has no
// disruption budget for the group.
func (s *StateStore) allocDisruptionStatusTxn(txn Txn, alloc *structs.Allocation) (*structs.DisruptionStatus, error) {
	job, err := s.JobByIDTxn(nil, alloc.Namespace, alloc.JobID, txn)
	if err != nil {
		return nil, err
	}
	if job == nil || job.Type != structs.JobTypeService {
		return nil, nil
	}
	tg := job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil || tg.Disruption == nil {
		return nil, nil
	}
	statuses, err := disruptionStatusTxn(txn, nil, job)
	if err != nil {
		return nil, err
	}
	return statuses[tg.Name], nil
}

// DisruptionStatusByJob returns the disruption budget status of each task
// group of the job that has a disruption block, and the index to use for
// blocking queries. It returns a nil map if the job does not exist.
func (s *StateStore) DisruptionStatusByJob(ws memdb.WatchSet, namespace, jobID string) (map[string]*structs.DisruptionStatus, uint64, error) {
	txn := s.db.ReadTxn()

	var index uint64
	for _, table := range []string{"jobs", "allocs", "deployment"} {
		raw, err := txn.First("index", "id", table)
		if err != nil {
			return nil, 0, err
		}
		if raw != nil {
			index = max(index, raw.(*IndexEntry).Value)
		}
	}

	job, err := s.JobByIDTxn(ws, namespace, jobID, txn)
	if err != nil || job == nil {
		return nil, index, err
	}
	out, err := disruptionStatusTxn(txn, ws, job)
	return out, index, err
}

// disruptionStatusTxn computes the disruption budget status of each task
// group of the job that has a disruption block, from the allocations of the
// job (with the job's create index) and its latest deployment.
func disruptionStatusTxn(txn Txn, ws memdb.WatchSet, job *structs.Job) (map[string]*structs.DisruptionStatus, error) {
	out := make(map[string]*structs.DisruptionStatus)
	if !job.UsesDisruptionBudgets() {
		return out, nil
	}

	iter, err := txn.Get("allocs", "job", job.Namespace, job.ID)
	if err != nil {
		return nil, err
	}
	ws.Add(iter.WatchCh())
	var allocs []*structs.Allocation
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		alloc := raw.(*structs.Allocation)
		if alloc.Job != nil && alloc.Job.CreateIndex != job.CreateIndex {
			continue
		}
		allocs = append(allocs, alloc)
	}

	dIter, err := txn.Get("deployment", "job", job.Namespace, job.ID)
	if err != nil {
		return nil, err
	}
	ws.Add(dIter.WatchCh())
	var deployment *structs.Deployment
	for raw := dIter.Next(); raw != nil; raw = dIter.Next() {
		d := raw.(*structs.Deployment)
		if deployment == nil || deployment.CreateIndex < d.CreateIndex {
			deployment = d
		}
	}

	for _, tg := range job.TaskGroups {
		if status := structs.NewDisruptionStatus(tg, allocs, deployment); status != nil {
			out[tg.Name] = status
		}
	}
	return out, nil
}
