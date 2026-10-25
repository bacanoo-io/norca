// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/hashicorp/nomad/controller/fake"
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
	le, err := controller.NewLeaderElector(b, leaseConfig(id), testLogger(t))
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

func TestLeaderElection_Config(t *testing.T) {
	b := fake.New()
	_, err := controller.NewLeaderElector(b, controller.LeaderElectionConfig{}, nil)
	must.Error(t, err)
	_, err = controller.NewLeaderElector(b, controller.LeaderElectionConfig{
		Path: "x", LeaseDuration: time.Second, RenewDeadline: 2 * time.Second,
	}, nil)
	must.Error(t, err)
}
