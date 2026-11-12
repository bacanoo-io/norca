// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// ScaleJob sets the count of one task group of the stored job. It is what
// scaling does to a job (Job.Scale: the scale API, norca job scale, the
// autoscaler), as opposed to registering a new version of it:
//
//   - the version of the job and its JobModifyIndex are kept, so the
//     allocations that run are not updated, the version stays the stable one
//     it was, its submission stays with it, and scaling does not use up the
//     tracked versions of the job;
//   - no deployment is started by the change (the scheduler starts none for
//     allocations added to a version that already runs), and a deployment
//     of the job that is active goes on: the desired total of the group
//     follows the count, so the deployment waits for the allocations that
//     were added to it, and not for those that were taken away.
//
// The count is set on the job that is stored when the request is applied,
// whatever version that is, not on the copy the request was made from: a
// new version registered in between keeps its definition. Nothing is done,
// and nil is returned, when the job or the group is gone.
func (s *StateStore) ScaleJob(msgType structs.MessageType, index uint64, namespace, jobID, group string,
	count int, submitTime int64) (*structs.Job, error) {

	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	raw, err := txn.First("jobs", "id", namespace, jobID)
	if err != nil {
		return nil, fmt.Errorf("job lookup failed: %v", err)
	}
	if raw == nil {
		return nil, nil
	}
	job := raw.(*structs.Job).Copy()
	tg := job.LookupTaskGroup(group)
	if tg == nil {
		return nil, nil
	}
	delta := count - tg.Count
	tg.Count = count
	job.SubmitTime = submitTime

	if err := s.upsertJobImpl(index, nil, job, true, txn); err != nil {
		return nil, err
	}
	if err := s.scaleDeploymentTotal(index, job, group, delta, txn); err != nil {
		return nil, err
	}
	if err := txn.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

// scaleDeploymentTotal makes the desired total of a task group in the active
// deployment of the job's version follow a change of the group's count.
func (s *StateStore) scaleDeploymentTotal(index uint64, job *structs.Job, group string, delta int, txn *txn) error {
	if delta == 0 {
		return nil
	}
	iter, err := txn.Get("deployment", "job", job.Namespace, job.ID)
	if err != nil {
		return fmt.Errorf("deployment lookup failed: %v", err)
	}
	var latest *structs.Deployment
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		if d := raw.(*structs.Deployment); latest == nil || latest.CreateIndex < d.CreateIndex {
			latest = d
		}
	}
	if latest == nil || !latest.Active() || latest.JobCreateIndex != job.CreateIndex || latest.JobVersion != job.Version {
		return nil
	}
	if _, ok := latest.TaskGroups[group]; !ok {
		return nil
	}

	d := latest.Copy()
	dstate := d.TaskGroups[group]
	dstate.DesiredTotal = max(dstate.DesiredTotal+delta, 0)
	return s.upsertDeploymentImpl(index, d, txn)
}
