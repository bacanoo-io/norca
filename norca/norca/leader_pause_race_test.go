// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// A leadership change pauses or resumes workers while a reload replaces the
// worker pool. Under -race this found pausableWorkers reading s.workers
// without the worker lock; it must also not deadlock with the reload, which
// pauses the new workers while it holds the lock.
func TestServer_PausableWorkers_DuringReload(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 4
	})
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			reloadSchedulers(s1, &SchedulerWorkerPoolArgs{
				NumSchedulers:     2 + i%3,
				EnabledSchedulers: []string{structs.JobTypeCore, structs.JobTypeService, structs.JobTypeBatch},
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s1.handlePausableWorkers(i%2 == 0)
		}
	}()
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("reload and pausing the workers did not finish: deadlock")
	}

	must.Eq(t, 3, len(s1.GetSchedulerWorkersInfo())) // the last reload: 2 + 19%3
}
