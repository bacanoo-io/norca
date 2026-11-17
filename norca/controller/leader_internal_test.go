// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/shoenig/test/must"
)

// The candidates use the lease duration stored by the holder, so it must
// not lose its fraction of a second.
func TestLeaseRecord_DurationRoundTrip(t *testing.T) {
	for _, d := range []time.Duration{
		1500 * time.Millisecond,
		600 * time.Millisecond,
		20 * time.Second,
		time.Second + time.Nanosecond,
	} {
		items := LeaseRecord{Holder: "a", LeaseDuration: d}.items()
		must.Eq(t, d, leaseFromItems(items).LeaseDuration, must.Sprint(items[leaseItemDuration]))
	}
	must.Eq(t, "1.5", LeaseRecord{LeaseDuration: 1500 * time.Millisecond}.items()[leaseItemDuration])
	must.Eq(t, "20", LeaseRecord{LeaseDuration: 20 * time.Second}.items()[leaseItemDuration])

	// A record without a usable duration leaves it to the reader's own.
	for _, s := range []string{"", "x", "-1", "NaN", "+Inf", "1e300"} {
		r := leaseFromItems(api.VariableItems{leaseItemDuration: s})
		must.Eq(t, time.Duration(0), r.LeaseDuration, must.Sprint(s))
	}
}

// oneVar is a Variables API with a single Variable.
type oneVar struct {
	VariablesAPI
	index      uint64
	v          *api.Variable
	beforeRead func()
}

func (o *oneVar) put(items api.VariableItems) {
	o.index++
	o.v = &api.Variable{Namespace: "default", Path: "lease", ModifyIndex: o.index, Items: items}
}

func (o *oneVar) ReadVariable(context.Context, string, string) (*api.Variable, error) {
	if o.beforeRead != nil {
		o.beforeRead()
	}
	if o.v == nil {
		return nil, fmt.Errorf("variable lease: %w", ErrNotFound)
	}
	return o.v.Copy(), nil
}

func (o *oneVar) WriteVariable(_ context.Context, v *api.Variable) (*api.Variable, error) {
	if (o.v == nil) != (v.ModifyIndex == 0) || (o.v != nil && o.v.ModifyIndex != v.ModifyIndex) {
		return nil, fmt.Errorf("%w: variable lease", ErrConflict)
	}
	o.put(v.Items)
	return o.v.Copy(), nil
}

// A candidate counts the lease duration from a moment that is not before
// the renewal it saw: a renewal committed while the read was on its way
// must not be dated to the start of the read.
func TestLeaderElector_ObservesAfterTheRead(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1000, 0)
	vars := &oneVar{}
	le, err := NewLeaderElector(vars, LeaderElectionConfig{
		Path: "lease", Identity: "b",
		LeaseDuration: 10 * time.Second, RenewDeadline: 8 * time.Second, RetryPeriod: time.Second,
	}, nil)
	must.NoError(t, err)
	le.now = func() time.Time { return clock }

	held := LeaseRecord{Holder: "a", LeaseDuration: 10 * time.Second}
	vars.put(held.items())
	ok, err := le.tryAcquireOrRenew(ctx)
	must.NoError(t, err)
	must.False(t, ok)

	// The next read takes 3 seconds, and "a" renews just before the
	// servers answer it.
	vars.beforeRead = func() {
		clock = clock.Add(3 * time.Second)
		vars.put(held.items())
	}
	ok, err = le.tryAcquireOrRenew(ctx)
	must.NoError(t, err)
	must.False(t, ok)
	vars.beforeRead = nil

	// 9 seconds after that renewal the lease is still held.
	clock = clock.Add(9 * time.Second)
	ok, err = le.tryAcquireOrRenew(ctx)
	must.NoError(t, err)
	must.False(t, ok, must.Sprint("took the lease over 9s after its renewal, the lease duration is 10s"))

	clock = clock.Add(time.Second)
	ok, err = le.tryAcquireOrRenew(ctx)
	must.NoError(t, err)
	must.True(t, ok)
}
