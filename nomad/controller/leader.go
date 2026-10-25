// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/nomad/api"
)

// LeaseRoot is the default first path segment of lease Variables.
const LeaseRoot = "controller-lease"

// Items of a lease Variable.
const (
	leaseItemHolder        = "holder"
	leaseItemDuration      = "leaseDurationSeconds"
	leaseItemAcquireTime   = "acquireTime"
	leaseItemRenewTime     = "renewTime"
	leaseItemTransitions   = "transitions"
	defaultLeaseDuration   = 20 * time.Second
	defaultLeaseRenew      = 15 * time.Second
	defaultLeaseRetryEvery = 4 * time.Second
)

// LeaderElectionConfig configures a LeaderElector.
//
// Variables locks do not exist in this Nomad version, so the election uses a
// lease Variable written with check-and-set: whoever writes it successfully
// holds the lease and renews it every RetryPeriod. Candidates take over a
// lease when its Variable has not changed for LeaseDuration, measured with
// their own clock from the moment they observed the last change, so clock
// skew between the candidates does not matter.
type LeaderElectionConfig struct {
	// Namespace and Path of the lease Variable. Path defaults to
	// "controller-lease/<controller name>".
	Namespace string
	Path      string

	// Identity of this candidate; defaults to the allocation ID of the task
	// (NOMAD_ALLOC_ID), or host name and process ID.
	Identity string

	// LeaseDuration is how long candidates wait for a lease that is not
	// renewed before taking it over (default 20s).
	LeaseDuration time.Duration

	// RenewDeadline is how long the leader keeps leading without a
	// successful renewal (default 15s). It must be shorter than
	// LeaseDuration.
	RenewDeadline time.Duration

	// RetryPeriod is the interval of acquire and renew attempts (default
	// 4s).
	RetryPeriod time.Duration

	// ReleaseOnCancel releases the lease when the context of Run is
	// canceled, so another candidate can take over without waiting.
	ReleaseOnCancel bool
}

// LeaseRecord is the content of a lease Variable.
type LeaseRecord struct {
	Holder        string
	LeaseDuration time.Duration
	AcquireTime   time.Time
	RenewTime     time.Time
	Transitions   int
}

func leaseFromItems(items api.VariableItems) LeaseRecord {
	r := LeaseRecord{Holder: items[leaseItemHolder]}
	if s, err := strconv.Atoi(items[leaseItemDuration]); err == nil {
		r.LeaseDuration = time.Duration(s) * time.Second
	}
	r.AcquireTime, _ = time.Parse(time.RFC3339Nano, items[leaseItemAcquireTime])
	r.RenewTime, _ = time.Parse(time.RFC3339Nano, items[leaseItemRenewTime])
	r.Transitions, _ = strconv.Atoi(items[leaseItemTransitions])
	return r
}

func (r LeaseRecord) items() api.VariableItems {
	items := api.VariableItems{
		leaseItemDuration:    strconv.Itoa(int(r.LeaseDuration / time.Second)),
		leaseItemAcquireTime: r.AcquireTime.UTC().Format(time.RFC3339Nano),
		leaseItemRenewTime:   r.RenewTime.UTC().Format(time.RFC3339Nano),
		leaseItemTransitions: strconv.Itoa(r.Transitions),
	}
	// A released lease has no holder item.
	if r.Holder != "" {
		items[leaseItemHolder] = r.Holder
	}
	return items
}

// LeaderElector runs a leader election over a lease Variable.
type LeaderElector struct {
	cfg    LeaderElectionConfig
	vars   VariablesAPI
	logger *slog.Logger
	now    func() time.Time

	leader atomic.Bool

	mu            sync.Mutex
	observed      LeaseRecord
	observedIndex uint64
	observedTime  time.Time
}

// NewLeaderElector returns a leader elector. cfg.Path must be set.
func NewLeaderElector(vars VariablesAPI, cfg LeaderElectionConfig, logger *slog.Logger) (*LeaderElector, error) {
	if cfg.Path == "" {
		return nil, errors.New("leader election requires the path of the lease variable")
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.Identity == "" {
		cfg.Identity = defaultIdentity()
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = defaultLeaseDuration
	}
	if cfg.RenewDeadline <= 0 {
		cfg.RenewDeadline = defaultLeaseRenew
	}
	if cfg.RetryPeriod <= 0 {
		cfg.RetryPeriod = defaultLeaseRetryEvery
	}
	if cfg.RenewDeadline >= cfg.LeaseDuration {
		return nil, fmt.Errorf("the renew deadline (%s) must be shorter than the lease duration (%s)", cfg.RenewDeadline, cfg.LeaseDuration)
	}
	if cfg.RetryPeriod >= cfg.RenewDeadline {
		return nil, fmt.Errorf("the retry period (%s) must be shorter than the renew deadline (%s)", cfg.RetryPeriod, cfg.RenewDeadline)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LeaderElector{
		cfg:    cfg,
		vars:   vars,
		logger: logger.With("lease", cfg.Namespace+"/"+cfg.Path, "identity", cfg.Identity),
		now:    time.Now,
	}, nil
}

func defaultIdentity() string {
	for _, env := range []string{"NORCA_ALLOC_ID", "NOMAD_ALLOC_ID"} {
		if id := os.Getenv(env); id != "" {
			return id
		}
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// Identity returns the identity of this candidate.
func (le *LeaderElector) Identity() string { return le.cfg.Identity }

// IsLeader reports whether this candidate currently holds the lease.
func (le *LeaderElector) IsLeader() bool { return le.leader.Load() }

// Leader returns the holder of the lease as last observed.
func (le *LeaderElector) Leader() string {
	le.mu.Lock()
	defer le.mu.Unlock()
	return le.observed.Holder
}

// Run campaigns until ctx is canceled. Whenever this candidate becomes the
// leader, onStarted is called with a context that is canceled when the
// leadership is lost; Run waits for onStarted to return, calls onStopped,
// and campaigns again.
func (le *LeaderElector) Run(ctx context.Context, onStarted func(ctx context.Context), onStopped func()) {
	for ctx.Err() == nil {
		if !le.acquire(ctx) {
			break
		}
		leaderCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			onStarted(leaderCtx)
		}()
		le.renew(leaderCtx)
		le.leader.Store(false)
		cancel()
		<-done
		if onStopped != nil {
			onStopped()
		}
	}
	if le.cfg.ReleaseOnCancel {
		le.release()
	}
}

// acquire tries to acquire the lease every RetryPeriod until it succeeds
// (true) or ctx is canceled (false).
func (le *LeaderElector) acquire(ctx context.Context) bool {
	logged := false
	for {
		ok, err := le.tryAcquireOrRenew(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			le.logger.Warn("acquiring the lease failed", "error", err)
		case ok:
			le.leader.Store(true)
			le.logger.Info("acquired the lease")
			return true
		case !logged:
			le.logger.Info("waiting for the lease", "leader", le.Leader())
			logged = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(le.cfg.RetryPeriod):
		}
	}
}

// renew renews the lease every RetryPeriod; it returns when a renewal did
// not succeed within RenewDeadline, the lease was taken over, or ctx is
// canceled.
func (le *LeaderElector) renew(ctx context.Context) {
	lastRenew := le.now()
	ticker := time.NewTicker(le.cfg.RetryPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		attemptCtx, cancel := context.WithTimeout(ctx, le.cfg.RenewDeadline)
		ok, err := le.tryAcquireOrRenew(attemptCtx)
		cancel()
		switch {
		case ok:
			lastRenew = le.now()
			continue
		case err == nil:
			le.logger.Warn("lost the lease", "leader", le.Leader())
			return
		case ctx.Err() != nil:
			return
		}
		le.logger.Warn("renewing the lease failed", "error", err)
		if le.now().Sub(lastRenew) >= le.cfg.RenewDeadline {
			le.logger.Warn("renew deadline exceeded, giving up the leadership")
			return
		}
	}
}

// tryAcquireOrRenew makes one attempt. It returns true if this candidate
// holds the lease afterwards, false with a nil error if another candidate
// holds it.
func (le *LeaderElector) tryAcquireOrRenew(ctx context.Context) (bool, error) {
	now := le.now()
	desired := LeaseRecord{
		Holder:        le.cfg.Identity,
		LeaseDuration: le.cfg.LeaseDuration,
		AcquireTime:   now,
		RenewTime:     now,
	}

	v, err := le.vars.ReadVariable(ctx, le.cfg.Namespace, le.cfg.Path)
	if errors.Is(err, ErrNotFound) {
		out, err := le.vars.WriteVariable(ctx, &api.Variable{
			Namespace: le.cfg.Namespace,
			Path:      le.cfg.Path,
			Items:     desired.items(),
		})
		if errors.Is(err, ErrConflict) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		le.observe(desired, out.ModifyIndex, now)
		return true, nil
	} else if err != nil {
		return false, err
	}

	cur := leaseFromItems(v.Items)
	le.mu.Lock()
	if v.ModifyIndex != le.observedIndex {
		le.observed, le.observedIndex, le.observedTime = cur, v.ModifyIndex, now
	}
	observedTime := le.observedTime
	le.mu.Unlock()

	duration := cur.LeaseDuration
	if duration <= 0 {
		duration = le.cfg.LeaseDuration
	}
	if cur.Holder != "" && cur.Holder != le.cfg.Identity && now.Before(observedTime.Add(duration)) {
		return false, nil
	}

	if cur.Holder == le.cfg.Identity {
		desired.AcquireTime = cur.AcquireTime
		desired.Transitions = cur.Transitions
	} else {
		desired.Transitions = cur.Transitions + 1
	}
	out, err := le.vars.WriteVariable(ctx, &api.Variable{
		Namespace:   le.cfg.Namespace,
		Path:        le.cfg.Path,
		ModifyIndex: v.ModifyIndex,
		Items:       desired.items(),
	})
	if errors.Is(err, ErrConflict) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	le.observe(desired, out.ModifyIndex, now)
	return true, nil
}

func (le *LeaderElector) observe(r LeaseRecord, index uint64, now time.Time) {
	le.mu.Lock()
	defer le.mu.Unlock()
	le.observed, le.observedIndex, le.observedTime = r, index, now
}

// release gives up the lease if this candidate still holds it, by clearing
// the holder with check-and-set.
func (le *LeaderElector) release() {
	ctx, cancel := context.WithTimeout(context.Background(), le.cfg.RetryPeriod)
	defer cancel()
	v, err := le.vars.ReadVariable(ctx, le.cfg.Namespace, le.cfg.Path)
	if err != nil {
		return
	}
	cur := leaseFromItems(v.Items)
	if cur.Holder != le.cfg.Identity {
		return
	}
	cur.Holder = ""
	cur.RenewTime = le.now()
	_, err = le.vars.WriteVariable(ctx, &api.Variable{
		Namespace:   le.cfg.Namespace,
		Path:        le.cfg.Path,
		ModifyIndex: v.ModifyIndex,
		Items:       cur.items(),
	})
	if err == nil {
		le.logger.Info("released the lease")
	}
}
