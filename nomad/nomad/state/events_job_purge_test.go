// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// TestEventsFromChanges_JobPurge asserts that purging a job publishes a Job
// event that carries the last known version of the job.
func TestEventsFromChanges_JobPurge(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	job := mock.Job()
	must.NoError(t, s.UpsertJob(structs.JobRegisterRequestType, 10, nil, job))

	// Job.Deregister with purge deletes the job in the FSM's transaction.
	txn := s.db.WriteTxnMsgT(structs.JobDeregisterRequestType, 20)
	must.NoError(t, s.DeleteJobTxn(20, job.Namespace, job.ID, txn))
	must.NoError(t, txn.Commit())

	events := WaitForEvents(t, s, 20, 1, time.Second)
	var jobEvents []structs.Event
	for _, ev := range events {
		if ev.Topic == structs.TopicJob {
			jobEvents = append(jobEvents, ev)
		}
	}
	must.Len(t, 1, jobEvents)

	got := jobEvents[0]
	must.Eq(t, structs.TypeJobDeregistered, got.Type)
	must.Eq(t, uint64(20), got.Index)
	must.Eq(t, job.ID, got.Key)
	must.Eq(t, job.Namespace, got.Namespace)
	payload, ok := got.Payload.(*structs.JobEvent)
	must.True(t, ok)
	must.Eq(t, job.ID, payload.Job.ID)
	must.Eq(t, uint64(10), payload.Job.ModifyIndex)

	gone, err := s.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Nil(t, gone)
}

// TestEventsFromChanges_JobBatchPurge asserts that jobs deleted by the job
// garbage collector publish Job events too.
func TestEventsFromChanges_JobBatchPurge(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	job := mock.BatchJob()
	must.NoError(t, s.UpsertJob(structs.JobRegisterRequestType, 10, nil, job))

	txn := s.db.WriteTxnMsgT(structs.JobBatchDeregisterRequestType, 20)
	must.NoError(t, s.DeleteJobTxn(20, job.Namespace, job.ID, txn))
	must.NoError(t, txn.Commit())

	events := WaitForEvents(t, s, 20, 1, time.Second)
	var found bool
	for _, ev := range events {
		if ev.Topic == structs.TopicJob && ev.Key == job.ID {
			found = true
			must.Eq(t, structs.TypeJobBatchDeregistered, ev.Type)
		}
	}
	must.True(t, found)
}
