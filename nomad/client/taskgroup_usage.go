// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"sync"
	"time"

	"github.com/hashicorp/nomad/client/allocrunner"
	"github.com/hashicorp/nomad/nomad/structs"
)

// taskGroupUsageReporter aggregates the resource usage of the client's task
// groups for the built-in autoscaler of the servers, and holds the finished
// report until allocSync sends it with the next Node.UpdateAlloc.
//
// The servers enable it through the heartbeat response
// (NodeUpdateResponse.TaskGroupUsageInterval). While the interval is zero,
// nothing is collected or sent, so clients talking to old servers or to a
// cluster without the built-in autoscaler behave as before.
type taskGroupUsageReporter struct {
	agg *allocrunner.TaskGroupUsageAggregator

	l           sync.Mutex
	interval    time.Duration
	windowStart time.Time
	pending     *structs.TaskGroupUsageReport
}

func newTaskGroupUsageReporter() *taskGroupUsageReporter {
	return &taskGroupUsageReporter{agg: allocrunner.NewTaskGroupUsageAggregator()}
}

// setInterval applies the aggregation window requested by the servers. A
// change starts a new window; zero disables the reporter.
func (r *taskGroupUsageReporter) setInterval(interval time.Duration, now time.Time) {
	if interval < 0 {
		interval = 0
	}

	r.l.Lock()
	defer r.l.Unlock()

	if interval == r.interval {
		return
	}
	r.interval = interval
	r.windowStart = now
	r.pending = nil
	r.agg.Reset()
}

// getInterval returns the aggregation window; zero when disabled.
func (r *taskGroupUsageReporter) getInterval() time.Duration {
	r.l.Lock()
	defer r.l.Unlock()
	return r.interval
}

// windowDone closes the current window if it has elapsed, and stores its
// report for allocSync. A report that was not sent yet is replaced: only
// the latest window is worth sending.
func (r *taskGroupUsageReporter) windowDone(nodeID string, now time.Time) {
	r.l.Lock()
	defer r.l.Unlock()

	if r.interval <= 0 || now.Sub(r.windowStart) < r.interval {
		return
	}
	window := now.Sub(r.windowStart)
	r.windowStart = now

	groups := r.agg.Flush()
	if len(groups) == 0 {
		return
	}
	r.pending = &structs.TaskGroupUsageReport{
		NodeID: nodeID,
		Window: window,
		Groups: groups,
	}
}

// take returns the finished report waiting to be sent, if any, and clears
// it. A report that fails to send is dropped, not retried.
func (r *taskGroupUsageReporter) take() *structs.TaskGroupUsageReport {
	r.l.Lock()
	defer r.l.Unlock()
	p := r.pending
	r.pending = nil
	return p
}

// collectTaskGroupUsage samples the resource usage of the running
// allocations every stats collection interval while the servers ask for
// task group usage, and closes an aggregation window when it has elapsed.
func (c *Client) collectTaskGroupUsage() {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-c.shutdownCh:
			return
		case <-timer.C:
		}

		sampleInterval := c.GetConfig().StatsCollectionInterval
		if sampleInterval <= 0 {
			sampleInterval = time.Second
		}

		interval := c.taskGroupUsage.getInterval()
		if interval <= 0 {
			timer.Reset(sampleInterval)
			continue
		}
		if sampleInterval > interval {
			sampleInterval = interval
		}

		for _, ar := range c.getAllocRunners() {
			if st := ar.AllocState(); st == nil || st.ClientStatus != structs.AllocClientStatusRunning {
				continue
			}
			usage, err := ar.StatsReporter().LatestAllocStats("")
			if err != nil {
				continue
			}
			c.taskGroupUsage.agg.Observe(ar.Alloc(), usage)
		}
		c.taskGroupUsage.windowDone(c.NodeID(), time.Now())

		timer.Reset(sampleInterval)
	}
}
