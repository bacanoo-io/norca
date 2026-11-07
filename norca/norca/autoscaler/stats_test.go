// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
)

// TestAutoscaler_Stats checks the activity the metrics store samples: none
// while the autoscaler does not run, then the queue, the policies and the
// suspensions that have not ended.
func TestAutoscaler_Stats(t *testing.T) {
	ci.Parallel(t)

	now := time.Now()
	a := New(hclog.NewNullLogger(), nil, &Deps{Now: func() time.Time { return now }})
	must.Eq(t, Stats{}, a.Stats())

	r := &runner{exec: &executor{suspended: map[targetKey]time.Time{
		{job: "a"}: now.Add(time.Minute),
		{job: "b"}: now.Add(-time.Minute),
	}}}
	r.queueLen.Store(2)
	r.policyCount.Store(5)
	a.runner.Store(r)
	must.Eq(t, Stats{Running: true, Queue: 2, Policies: 5, Suspended: 1}, a.Stats())

	a.runner.CompareAndSwap(r, nil)
	must.Eq(t, Stats{}, a.Stats())
}
