// Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package drainer

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// TestHandlePurgedJob covers the allocations of a purged job: one that still
// runs on a draining node keeps the job watched, one that stopped there is
// migrated (once: not again below the last handled index), and allocations
// on other nodes and of system jobs do not count.
func TestHandlePurgedJob(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	draining, running := testNodes(t, store)
	snap, err := store.Snapshot()
	must.NoError(t, err)

	alloc := func(node *structs.Node, status string, index uint64) *structs.Allocation {
		a := mock.Alloc()
		a.NodeID = node.ID
		a.ClientStatus = status
		a.ModifyIndex = index
		return a
	}
	system := alloc(draining, structs.AllocClientStatusRunning, 200)
	system.Job = mock.SystemJob()

	for name, tc := range map[string]struct {
		allocs   []*structs.Allocation
		last     uint64
		migrated int
		done     bool
	}{
		"running on the draining node": {
			allocs: []*structs.Allocation{alloc(draining, structs.AllocClientStatusRunning, 200)},
			last:   1, migrated: 0, done: false,
		},
		"stopped on the draining node": {
			allocs: []*structs.Allocation{alloc(draining, structs.AllocClientStatusComplete, 200)},
			last:   1, migrated: 1, done: true,
		},
		"stopped before the last handled index": {
			allocs: []*structs.Allocation{alloc(draining, structs.AllocClientStatusComplete, 200)},
			last:   300, migrated: 0, done: true,
		},
		"one stopped, one running": {
			allocs: []*structs.Allocation{
				alloc(draining, structs.AllocClientStatusComplete, 200),
				alloc(draining, structs.AllocClientStatusPending, 200),
			},
			last: 1, migrated: 1, done: false,
		},
		"running on a node that is not draining": {
			allocs: []*structs.Allocation{alloc(running, structs.AllocClientStatusRunning, 200)},
			last:   1, migrated: 0, done: true,
		},
		"system job": {
			allocs: []*structs.Allocation{system},
			last:   1, migrated: 0, done: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := handlePurgedJob(snap, tc.allocs, tc.last)
			must.NoError(t, err)
			must.Len(t, tc.migrated, r.migrated)
			must.Eq(t, tc.done, r.done)
			must.SliceEmpty(t, r.drain)
		})
	}
}

// TestDrainingJobWatcher_PurgedJob watches a job that is purged while its
// allocation runs on a draining node: nothing is reported until the client
// stops the allocation, which is then reported as migrated.
func TestDrainingJobWatcher_PurgedJob(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	jobWatcher, cancel := testDrainingJobWatcher(t, store)
	defer cancel()
	draining, _ := testNodes(t, store)

	job := mock.Job()
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 102, nil, job))
	a := mock.Alloc()
	a.Job = job
	a.JobID = job.ID
	a.TaskGroup = job.TaskGroups[0].Name
	a.NodeID = draining.ID
	a.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 103, []*structs.Allocation{a}))

	jobWatcher.RegisterJobs([]structs.NamespacedID{{Namespace: job.Namespace, ID: job.ID}})
	assertJobWatcherOps(t, jobWatcher, 0, 0)

	// The job is purged; its allocation still runs.
	must.NoError(t, store.DeleteJob(104, job.Namespace, job.ID))
	stopped := a.Copy()
	stopped.DesiredStatus = structs.AllocDesiredStatusStop
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 105, []*structs.Allocation{stopped}))
	assertJobWatcherOps(t, jobWatcher, 0, 0)

	// The client stops it.
	complete := stopped.Copy()
	complete.ClientStatus = structs.AllocClientStatusComplete
	must.NoError(t, store.UpdateAllocsFromClient(structs.MsgTypeTestSetup, 106, []*structs.Allocation{complete}))
	_, migrated := assertJobWatcherOps(t, jobWatcher, 0, 1)
	must.Eq(t, a.ID, migrated[0].ID)

	// The job is no longer watched.
	must.MapEmpty(t, jobWatcher.drainingJobs())
}
