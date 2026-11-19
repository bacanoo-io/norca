// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/controller"
	"github.com/bacanoo-io/norca/norca/controller/fake"
	"github.com/shoenig/test/must"
)

func leaseConfig(id string) controller.LeaderElectionConfig {
	return controller.LeaderElectionConfig{
		Path:            "controller-lease/test",
		Identity:        id,
		LeaseDuration:   600 * time.Millisecond,
		RenewDeadline:   400 * time.Millisecond,
		RetryPeriod:     100 * time.Millisecond,
		ReleaseOnCancel: true,
	}
}

type candidate struct {
	le      *controller.LeaderElector
	cancel  context.CancelFunc
	done    chan struct{}
	leading atomic.Bool
	terms   atomic.Int32
}

func startCandidate(t *testing.T, b controller.VariablesAPI, id string) *candidate {
	t.Helper()
	return startCandidateWith(t, b, leaseConfig(id))
}

func startCandidateWith(t *testing.T, b controller.VariablesAPI, cfg controller.LeaderElectionConfig) *candidate {
	t.Helper()
	le, err := controller.NewLeaderElector(b, cfg, testLogger(t))
	must.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	c := &candidate{le: le, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		le.Run(ctx, func(ctx context.Context) {
			c.terms.Add(1)
			c.leading.Store(true)
			<-ctx.Done()
			c.leading.Store(false)
		}, nil)
	}()
	t.Cleanup(c.stop)
	return c
}

func (c *candidate) stop() {
	c.cancel()
	<-c.done
}

// pausable wraps a backend and fails every call while paused, like a
// partitioned candidate.
type pausable struct {
	controller.VariablesAPI
	mu     sync.Mutex
	paused bool
}

func (p *pausable) set(paused bool) {
	p.mu.Lock()
	p.paused = paused
	p.mu.Unlock()
}

func (p *pausable) check() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		return context.DeadlineExceeded
	}
	return nil
}

func (p *pausable) ReadVariable(ctx context.Context, ns, path string) (*api.Variable, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return p.VariablesAPI.ReadVariable(ctx, ns, path)
}

func (p *pausable) WriteVariable(ctx context.Context, v *api.Variable) (*api.Variable, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return p.VariablesAPI.WriteVariable(ctx, v)
}

func TestLeaderElection_SingleLeaderAndFailover(t *testing.T) {
	b := fake.New()
	a := startCandidate(t, b, "a")
	waitFor(t, "a leads", a.leading.Load)

	c := startCandidate(t, b, "c")
	// c must not take over while a renews.
	time.Sleep(1500 * time.Millisecond)
	must.True(t, a.leading.Load())
	must.False(t, c.leading.Load())
	must.Eq(t, "a", c.le.Leader())

	// a stops and releases the lease: c takes over quickly.
	a.stop()
	waitFor(t, "c leads", c.leading.Load)
	must.Eq(t, "c", b.GetVariable("default", "controller-lease/test").Items["holder"])
	must.Eq(t, "1", b.GetVariable("default", "controller-lease/test").Items["transitions"])
}

func TestLeaderElection_TakeoverAfterExpiry(t *testing.T) {
	b := fake.New()
	pa := &pausable{VariablesAPI: b}
	a := startCandidate(t, pa, "a")
	waitFor(t, "a leads", a.leading.Load)
	c := startCandidate(t, b, "c")

	// a is partitioned: it gives up after the renew deadline, and c takes
	// over only after the lease duration.
	start := time.Now()
	pa.set(true)
	waitFor(t, "a steps down", func() bool { return !a.leading.Load() })
	waitFor(t, "c leads", c.leading.Load)
	must.Greater(t, 500*time.Millisecond, time.Since(start))

	// a comes back and does not lead while c renews.
	pa.set(false)
	time.Sleep(time.Second)
	must.False(t, a.leading.Load())
	must.True(t, c.leading.Load())
	must.Eq(t, int32(1), a.terms.Load())
}

// faulty wraps a backend whose calls fail at once or hang, like a server
// that restarts and then a write that waits for the raft log.
type faulty struct {
	controller.VariablesAPI

	// honorCtx makes a hanging call return when its context ends, as the
	// API client does; without it the call hangs until release is closed.
	honorCtx bool
	release  chan struct{}

	mu   sync.Mutex
	mode string
}

func (f *faulty) set(mode string) {
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
}

func (f *faulty) gate(ctx context.Context) error {
	f.mu.Lock()
	mode := f.mode
	f.mu.Unlock()
	switch mode {
	case "fail":
		return errors.New("connection refused")
	case "hang":
		if !f.honorCtx {
			<-f.release
			return errors.New("connection reset")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.release:
			return errors.New("connection reset")
		}
	}
	return nil
}

func (f *faulty) ReadVariable(ctx context.Context, ns, path string) (*api.Variable, error) {
	if err := f.gate(ctx); err != nil {
		return nil, err
	}
	return f.VariablesAPI.ReadVariable(ctx, ns, path)
}

func (f *faulty) WriteVariable(ctx context.Context, v *api.Variable) (*api.Variable, error) {
	if err := f.gate(ctx); err != nil {
		return nil, err
	}
	return f.VariablesAPI.WriteVariable(ctx, v)
}

// The renew deadline bounds the time since the last successful renewal,
// also while a renewal attempt is on its way: the leader steps down before
// a candidate may take the lease over. (Review finding CTL-1: the attempt
// that hangs got a whole renew deadline of its own, so the old leader led
// for up to twice the renew deadline, longer than the lease duration.)
func TestLeaderElection_HangingRenewalNoTwoLeaders(t *testing.T) {
	for name, honorCtx := range map[string]bool{"call returns with its context": true, "call ignores its context": false} {
		t.Run(name, func(t *testing.T) {
			config := func(id string) controller.LeaderElectionConfig {
				return controller.LeaderElectionConfig{
					Path:          "controller-lease/test",
					Identity:      id,
					LeaseDuration: time.Second,
					RenewDeadline: 900 * time.Millisecond,
					RetryPeriod:   100 * time.Millisecond,
				}
			}
			b := fake.New()
			fa := &faulty{VariablesAPI: b, honorCtx: honorCtx, release: make(chan struct{})}
			a := startCandidateWith(t, fa, config("a"))
			// Runs before the candidate is stopped, so a call that still
			// hangs does not block the cleanup.
			t.Cleanup(func() { close(fa.release) })
			waitFor(t, "a leads", a.leading.Load)
			c := startCandidateWith(t, b, config("c"))
			time.Sleep(300 * time.Millisecond)

			// The calls of a fail at once, then they hang.
			fa.set("fail")
			time.Sleep(750 * time.Millisecond)
			fa.set("hang")

			var (
				both      time.Duration
				bothSince time.Time
				giveUp    = time.Now().Add(4 * time.Second)
			)
			for time.Now().Before(giveUp) {
				aLeads, cLeads := a.leading.Load(), c.leading.Load()
				switch {
				case aLeads && cLeads && bothSince.IsZero():
					bothSince = time.Now()
				case !(aLeads && cLeads) && !bothSince.IsZero():
					both += time.Since(bothSince)
					bothSince = time.Time{}
				}
				if cLeads && !aLeads {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !bothSince.IsZero() {
				both += time.Since(bothSince)
			}
			must.True(t, c.leading.Load(), must.Sprint("c took the lease over"))
			if both > 0 {
				t.Fatalf("both leaders for %s", both.Round(time.Millisecond))
			}
			must.False(t, a.le.IsLeader())
		})
	}
}

// lateWrites wraps a backend whose lease writes are committed at once but
// answered only after delay, like a write that waits in the raft log, for
// the writes slow selects (numbered from 1).
type lateWrites struct {
	controller.VariablesAPI
	delay  time.Duration
	slow   func(n int32) bool
	writes atomic.Int32
}

func (l *lateWrites) WriteVariable(ctx context.Context, v *api.Variable) (*api.Variable, error) {
	n := l.writes.Add(1)
	out, err := l.VariablesAPI.WriteVariable(ctx, v)
	if l.slow(n) {
		time.Sleep(l.delay)
	}
	return out, err
}

// A lease write answered after the renew deadline does not make the
// candidate the leader: the other candidates count the lease duration from
// a moment before the answer came (review finding CON-6: the acquiring
// attempt had no deadline, and onStarted ran until renew noticed).
func TestLeaderElection_LateAcquireDoesNotLead(t *testing.T) {
	// The renew deadline is 400ms, the lease duration 600ms.
	run := func(t *testing.T, lw *lateWrites) (firstTerm *atomic.Int32, stop func()) {
		t.Helper()
		le, err := controller.NewLeaderElector(lw, leaseConfig("a"), testLogger(t))
		must.NoError(t, err)
		firstTerm = &atomic.Int32{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			le.Run(ctx, func(ctx context.Context) {
				// The number of lease writes when the first term began.
				firstTerm.CompareAndSwap(0, lw.writes.Load())
				<-ctx.Done()
			}, nil)
		}()
		var once sync.Once
		stop = func() {
			once.Do(func() {
				cancel()
				<-done
			})
		}
		t.Cleanup(stop)
		return firstTerm, stop
	}

	t.Run("the first write is late", func(t *testing.T) {
		lw := &lateWrites{VariablesAPI: fake.New(), delay: 500 * time.Millisecond, slow: func(n int32) bool { return n == 1 }}
		firstTerm, _ := run(t, lw)
		waitFor(t, "a leads", func() bool { return firstTerm.Load() != 0 })
		must.Greater(t, 1, firstTerm.Load(), must.Sprint("led on the write answered after the renew deadline"))
	})

	t.Run("every write is late", func(t *testing.T) {
		lw := &lateWrites{VariablesAPI: fake.New(), delay: 500 * time.Millisecond, slow: func(int32) bool { return true }}
		firstTerm, stop := run(t, lw)
		time.Sleep(1500 * time.Millisecond)
		stop()
		must.Eq(t, int32(0), firstTerm.Load(), must.Sprint("led on a write answered after the renew deadline"))
		must.Greater(t, 1, lw.writes.Load())
	})
}

// A lease duration with a fraction of a second reaches the candidates
// whole (review finding CTL-2: 1.5s was stored as "1").
func TestLeaderElection_FractionalLeaseDuration(t *testing.T) {
	b := fake.New()
	a := startCandidateWith(t, b, controller.LeaderElectionConfig{
		Path:          "controller-lease/test",
		Identity:      "a",
		LeaseDuration: 1500 * time.Millisecond,
		RenewDeadline: 1200 * time.Millisecond,
		RetryPeriod:   300 * time.Millisecond,
	})
	waitFor(t, "a leads", a.leading.Load)
	must.Eq(t, "1.5", b.GetVariable("default", "controller-lease/test").Items["leaseDurationSeconds"])
}

func TestLeaderElection_Config(t *testing.T) {
	b := fake.New()
	_, err := controller.NewLeaderElector(b, controller.LeaderElectionConfig{}, nil)
	must.Error(t, err)
	_, err = controller.NewLeaderElector(b, controller.LeaderElectionConfig{
		Path: "x", LeaseDuration: time.Second, RenewDeadline: 2 * time.Second,
	}, nil)
	must.Error(t, err)
}
