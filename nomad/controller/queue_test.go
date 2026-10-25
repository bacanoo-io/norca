// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"testing"
	"time"

	"github.com/shoenig/test/must"
)

func TestQueue_Dedup(t *testing.T) {
	q := NewQueue[string](nil)
	defer q.ShutDown()

	q.Add("a")
	q.Add("b")
	q.Add("a")
	must.Eq(t, 2, q.Len())

	k, shutdown := q.Get()
	must.False(t, shutdown)
	must.Eq(t, "a", k)

	// Added while processing: not handed out again until Done.
	q.Add("a")
	must.Eq(t, 1, q.Len())
	k, _ = q.Get()
	must.Eq(t, "b", k)
	must.Eq(t, 0, q.Len())
	q.Done("a")
	must.Eq(t, 1, q.Len())
	k, _ = q.Get()
	must.Eq(t, "a", k)
	q.Done("a")
	q.Done("b")
	must.Eq(t, 0, q.Len())
}

func TestQueue_AddAfter(t *testing.T) {
	q := NewQueue[string](nil)
	defer q.ShutDown()

	q.AddAfter("late", 200*time.Millisecond)
	q.AddAfter("soon", 20*time.Millisecond)
	q.AddAfter("late", time.Hour) // the earlier time wins
	must.Eq(t, 0, q.Len())
	must.Eq(t, 2, q.Waiting())

	start := time.Now()
	k, _ := q.Get()
	must.Eq(t, "soon", k)
	k, _ = q.Get()
	must.Eq(t, "late", k)
	must.Greater(t, 150*time.Millisecond, time.Since(start))
}

func TestQueue_ShutDown(t *testing.T) {
	q := NewQueue[string](nil)
	done := make(chan bool)
	go func() {
		_, shutdown := q.Get()
		done <- shutdown
	}()
	time.Sleep(20 * time.Millisecond)
	q.ShutDown()
	select {
	case shutdown := <-done:
		must.True(t, shutdown)
	case <-time.After(time.Second):
		t.Fatal("Get did not return after ShutDown")
	}
	q.Add("x")
	must.Eq(t, 0, q.Len())
	must.True(t, q.ShuttingDown())
}

func TestQueue_RateLimited(t *testing.T) {
	q := NewQueue[string](NewExponentialBackoff[string](10*time.Millisecond, 40*time.Millisecond))
	defer q.ShutDown()

	q.AddRateLimited("a")
	q.AddRateLimited("a")
	must.Eq(t, 2, q.NumRequeues("a"))
	k, _ := q.Get()
	must.Eq(t, "a", k)
	q.Done("a")
	q.Forget("a")
	must.Eq(t, 0, q.NumRequeues("a"))
}

func TestExponentialBackoff(t *testing.T) {
	r := NewExponentialBackoff[string](time.Second, 5*time.Second)
	must.Eq(t, time.Second, r.When("a"))
	must.Eq(t, 2*time.Second, r.When("a"))
	must.Eq(t, 4*time.Second, r.When("a"))
	must.Eq(t, 5*time.Second, r.When("a"))
	must.Eq(t, time.Second, r.When("b"))
	r.Forget("a")
	must.Eq(t, time.Second, r.When("a"))
}

func TestTokenBucket(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := NewTokenBucket[string](10, 2)
	r.now = func() time.Time { return now }
	must.Eq(t, time.Duration(0), r.When("a"))
	must.Eq(t, time.Duration(0), r.When("b"))
	must.Eq(t, 100*time.Millisecond, r.When("c"))
	must.Eq(t, 200*time.Millisecond, r.When("d"))
	now = now.Add(time.Second)
	must.Eq(t, time.Duration(0), r.When("e"))

	m := MaxOf[string]{NewExponentialBackoff[string](time.Second, time.Minute), r}
	must.Eq(t, time.Second, m.When("x"))
	must.Eq(t, 1, m.NumRequeues("x"))
}
