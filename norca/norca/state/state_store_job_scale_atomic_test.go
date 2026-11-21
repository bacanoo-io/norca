// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// A scaling writes the count, its evaluation and its scaling event in one
// transaction, at one index (review finding ASC-9: three Raft writes, and a
// failure between them left a count without its evaluation or its event).
func TestStateStore_ScaleJobWithEval(t *testing.T) {
	ci.Parallel(t)

	s := testStateStore(t)
	job := mock.Job()
	must.NoError(t, s.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))
	group := job.TaskGroups[0].Name

	eval := mock.Eval()
	eval.JobID, eval.Namespace = job.ID, job.Namespace
	event := &structs.ScalingEventRequest{
		Namespace: job.Namespace, JobID: job.ID, TaskGroup: group,
		ScalingEvent: &structs.ScalingEvent{PreviousCount: int64(job.TaskGroups[0].Count), Count: pointer.Of(int64(7)),
			EvalID: &eval.ID},
	}
	scaled, err := s.ScaleJobWithEval(structs.MsgTypeTestSetup, 1001, job.Namespace, job.ID, group, 7, 0, eval, event)
	must.NoError(t, err)
	must.Eq(t, 7, scaled.LookupTaskGroup(group).Count)

	stored, err := s.EvalByID(nil, eval.ID)
	must.NoError(t, err)
	must.NotNil(t, stored)
	must.Eq(t, uint64(1001), stored.CreateIndex)
	must.Eq(t, uint64(1001), stored.JobModifyIndex)
	events, _, err := s.ScalingEventsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 1, events[group])
	must.Eq(t, uint64(1001), events[group][0].CreateIndex)

	// A job that is gone takes nothing of the scaling.
	gone := mock.Eval()
	_, err = s.ScaleJobWithEval(structs.MsgTypeTestSetup, 1002, job.Namespace, "nope", group, 3, 0, gone, nil)
	must.NoError(t, err)
	stored, err = s.EvalByID(nil, gone.ID)
	must.NoError(t, err)
	must.Nil(t, stored)
}
