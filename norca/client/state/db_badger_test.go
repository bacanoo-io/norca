// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	trstate "github.com/bacanoo-io/norca/norca/client/allocrunner/taskrunner/state"
	"github.com/bacanoo-io/norca/norca/helper/badgerdb"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/dgraph-io/badger/v4"
	"github.com/shoenig/test/must"
)

// TestBadgerStateDB_reopen asserts the state survives closing and opening
// the database again.
func TestBadgerStateDB_reopen(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()

	db, err := OpenBadgerStateDB(testlog.HCLogger(t), dir, badgerdb.Options{})
	must.NoError(t, err)
	alloc := mock.Alloc()
	must.NoError(t, db.PutAllocation(alloc))
	must.NoError(t, db.PutTaskState(alloc.ID, "web", &structs.TaskState{State: structs.TaskStateRunning}))
	must.NoError(t, db.PutNodeMeta(map[string]*string{"a": nil}))
	must.NoError(t, db.Close())
	must.True(t, badgerdb.IsDB(filepath.Join(dir, stateDirName)))

	db, err = OpenBadgerStateDB(testlog.HCLogger(t), dir, badgerdb.Options{})
	must.NoError(t, err)
	defer db.Close()
	allocs, errs, err := db.GetAllAllocations()
	must.NoError(t, err)
	must.MapEmpty(t, errs)
	must.Len(t, 1, allocs)
	must.Eq(t, alloc.ID, allocs[0].ID)
	_, ts, err := db.GetTaskRunnerState(alloc.ID, "web")
	must.NoError(t, err)
	must.Eq(t, structs.TaskStateRunning, ts.State)
	meta, err := db.GetNodeMeta()
	must.NoError(t, err)
	must.MapContainsKey(t, meta, "a")
}

// TestBadgerStateDB_dedup asserts an unchanged value is not written again,
// and that a delete forgets the value so that it is written once more.
func TestBadgerStateDB_dedup(t *testing.T) {
	ci.Parallel(t)
	db := setupBadgerStateDB(t)

	version := func(key []byte) uint64 {
		var v uint64
		must.NoError(t, db.db.View(func(txn *badger.Txn) error {
			item, err := txn.Get(key)
			if err != nil {
				return err
			}
			v = item.Version()
			return nil
		}))
		return v
	}

	ls := &trstate.LocalState{Hooks: map[string]*trstate.HookState{"h": {PrestartDone: true}}}
	key := taskKeyOf("alloc", "task", taskNameLocalState)
	must.NoError(t, db.PutTaskRunnerLocalState("alloc", "task", ls))
	first := version(key)
	must.NoError(t, db.PutTaskRunnerLocalState("alloc", "task", ls))
	must.Eq(t, first, version(key)) // skipped

	ls.Hooks["h"].PrestartDone = false
	must.NoError(t, db.PutTaskRunnerLocalState("alloc", "task", ls))
	second := version(key)
	must.Greater(t, first, second)

	must.NoError(t, db.DeleteTaskBucket("alloc", "task"))
	got, _, err := db.GetTaskRunnerState("alloc", "task")
	must.NoError(t, err)
	must.Nil(t, got)
	must.NoError(t, db.PutTaskRunnerLocalState("alloc", "task", ls))
	got, _, err = db.GetTaskRunnerState("alloc", "task")
	must.NoError(t, err)
	must.NotNil(t, got)
}

// TestBadgerStateDB_prefixes asserts deletes stay within their task or
// allocation, also for names that are prefixes of each other.
func TestBadgerStateDB_prefixes(t *testing.T) {
	ci.Parallel(t)
	db := setupBadgerStateDB(t)
	ts := &structs.TaskState{State: structs.TaskStatePending}

	must.NoError(t, db.PutTaskState("a1", "web", ts))
	must.NoError(t, db.PutTaskState("a1", "web2", ts))
	must.NoError(t, db.PutTaskState("a11", "web", ts))
	must.NoError(t, db.PutCheckResult("a1", &structs.CheckQueryResult{ID: "c"}))
	must.NoError(t, db.PutCheckResult("a11", &structs.CheckQueryResult{ID: "c"}))

	must.NoError(t, db.DeleteTaskBucket("a1", "web"))
	_, got, err := db.GetTaskRunnerState("a1", "web2")
	must.NoError(t, err)
	must.NotNil(t, got)

	must.NoError(t, db.DeleteAllocationBucket("a1"))
	must.NoError(t, db.PurgeCheckResults("a1"))
	_, got, err = db.GetTaskRunnerState("a1", "web2")
	must.NoError(t, err)
	must.Nil(t, got)
	_, got, err = db.GetTaskRunnerState("a11", "web")
	must.NoError(t, err)
	must.NotNil(t, got)
	results, err := db.GetCheckResults()
	must.NoError(t, err)
	must.MapContainsKeys(t, results, []string{"a11"})
	must.MapNotContainsKey(t, results, "a1")

	// an allocation with state but without the allocation itself is an error
	allocs, errs, err := db.GetAllAllocations()
	must.NoError(t, err)
	must.SliceEmpty(t, allocs)
	must.MapContainsKey(t, errs, "a11")
}

// TestBadgerStateDB_inUse asserts a second agent cannot open the state.
func TestBadgerStateDB_inUse(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	db, err := OpenBadgerStateDB(testlog.HCLogger(t), dir, badgerdb.Options{})
	must.NoError(t, err)
	defer db.Close()

	_, err = OpenBadgerStateDB(testlog.HCLogger(t), dir, badgerdb.Options{})
	must.ErrorContains(t, err, "another norca agent")
}

// TestBadgerStateDB_boltState asserts the BoltDB state of an older Norca is
// reported instead of being ignored.
func TestBadgerStateDB_boltState(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "state.db"), []byte("bolt"), 0o600))
	_, err := OpenBadgerStateDB(testlog.HCLogger(t), dir, badgerdb.Options{})
	must.ErrorContains(t, err, "BoltDB client state")
}

// TestGetStateDBFactory asserts -dev agents keep no state and the others
// open Badger with the given options.
func TestGetStateDBFactory(t *testing.T) {
	ci.Parallel(t)

	db, err := GetStateDBFactory(true, badgerdb.Options{})(testlog.HCLogger(t), t.TempDir())
	must.NoError(t, err)
	must.Eq(t, "noopdb", db.Name())

	db, err = GetStateDBFactory(false, badgerdb.Options{MemTableSizeMB: 8})(testlog.HCLogger(t), t.TempDir())
	must.NoError(t, err)
	defer db.Close()
	must.Eq(t, "badger", db.Name())
	must.Eq(t, 8, db.(*BadgerStateDB).db.Opts.MemTableSizeMB)
	must.Eq(t, badgerdb.ClientDefaults.NumCompactors, db.(*BadgerStateDB).db.Opts.NumCompactors)
}
