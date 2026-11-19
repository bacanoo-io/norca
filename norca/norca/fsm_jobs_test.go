// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"
)

// The check-and-set of a job registration (EnforceIndex, JobModifyIndex)
// holds when the registration is applied, not only on the snapshot the
// endpoint read (review finding ATM-2): a registration whose job changed or
// was created in between is refused, and nothing of it is stored.
func TestFSM_RegisterJob_EnforceIndex(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	apply := func(index uint64, req *structs.JobRegisterRequest) any {
		t.Helper()
		buf, err := structs.Encode(structs.JobRegisterRequestType, req)
		must.NoError(t, err)
		return fsm.Apply(&raft.Log{Index: index, Term: 1, Type: raft.LogCommand, Data: buf})
	}
	register := func(job *structs.Job, enforce bool, jobModifyIndex uint64) *structs.JobRegisterRequest {
		eval := mock.Eval()
		eval.ID, eval.JobID, eval.Namespace = uuid.Generate(), job.ID, job.Namespace
		return &structs.JobRegisterRequest{
			Job:            job.Copy(),
			Eval:           eval,
			EnforceIndex:   enforce,
			JobModifyIndex: jobModifyIndex,
			WriteRequest:   structs.WriteRequest{Namespace: job.Namespace},
		}
	}
	stored := func(id string) *structs.Job {
		t.Helper()
		job, err := fsm.State().JobByID(nil, structs.DefaultNamespace, id)
		must.NoError(t, err)
		return job
	}
	refused := func(index uint64, req *structs.JobRegisterRequest) {
		t.Helper()
		resp := apply(index, req)
		err, ok := resp.(error)
		must.True(t, ok, must.Sprintf("the registration was applied: %v", resp))
		must.ErrorContains(t, err, RegisterEnforceIndexErrPrefix)
		eval, err := fsm.State().EvalByID(nil, req.Eval.ID)
		must.NoError(t, err)
		must.Nil(t, eval, must.Sprint("the evaluation of a refused registration was stored"))
	}

	job := mock.Job()
	must.Nil(t, apply(10, register(job, false, 0)))
	must.Eq(t, 10, stored(job.ID).JobModifyIndex)

	// Read at index 9, changed since: refused, the stored job stays.
	changed := job.Copy()
	changed.Meta = map[string]string{"changed": "yes"}
	refused(11, register(changed, true, 9))
	must.Eq(t, 10, stored(job.ID).JobModifyIndex)
	must.MapNotContainsKey(t, stored(job.ID).Meta, "changed")

	// Absent when read, created since: refused.
	refused(12, register(changed, true, 0))
	must.Eq(t, 10, stored(job.ID).JobModifyIndex)

	// Read at the stored index: applied.
	must.Nil(t, apply(13, register(changed, true, 10)))
	must.Eq(t, 13, stored(job.ID).JobModifyIndex)
	must.Eq(t, "yes", stored(job.ID).Meta["changed"])

	// A job read at an index that is gone since: refused.
	other := mock.Job()
	refused(14, register(other, true, 5))
	must.Nil(t, stored(other.ID))
	must.Nil(t, apply(15, register(other, true, 0)))
	must.Eq(t, 15, stored(other.ID).JobModifyIndex)

	// A scaling sets the count on the stored job whatever its index: its
	// EnforceIndex is not a check of the stored job's JobModifyIndex.
	scaled := stored(job.ID).Copy()
	scaled.TaskGroups[0].Count = 3
	scale := register(scaled, true, 1)
	scale.ScaleGroup, scale.Eval = scaled.TaskGroups[0].Name, nil
	must.Nil(t, apply(16, scale))
	must.Eq(t, 3, stored(job.ID).TaskGroups[0].Count)
}
