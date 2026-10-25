// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package discovery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// FakeCatalog serves ServiceRegistration.GetService from memory, with
// blocking queries, per region.
type FakeCatalog struct {
	lock    sync.Mutex
	cond    *sync.Cond
	index   uint64
	regions map[string][]*structs.ServiceRegistration
	calls   int
	tokens  []string
}

func NewFakeCatalog() *FakeCatalog {
	f := &FakeCatalog{index: 1, regions: make(map[string][]*structs.ServiceRegistration)}
	f.cond = sync.NewCond(&f.lock)
	return f
}

// Set replaces the registrations of a region.
func (f *FakeCatalog) Set(region string, regs ...*structs.ServiceRegistration) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.index++
	for _, reg := range regs {
		reg.ModifyIndex = f.index
	}
	f.regions[region] = regs
	f.cond.Broadcast()
}

func (f *FakeCatalog) RPC(method string, args, reply any) error {
	if method != structs.ServiceRegistrationGetServiceRPCMethod {
		return errors.New("unexpected method " + method)
	}
	req := args.(*structs.ServiceRegistrationByNameRequest)
	resp := reply.(*structs.ServiceRegistrationByNameResponse)

	f.lock.Lock()
	defer f.lock.Unlock()
	f.calls++
	f.tokens = append(f.tokens, req.AuthToken)

	if _, ok := f.regions[req.Region]; !ok && req.Region != "eu" {
		return errors.New("No path to region")
	}

	deadline := time.Now().Add(req.MaxQueryTime)
	for req.MinQueryIndex >= f.index && time.Now().Before(deadline) {
		go func() { time.Sleep(10 * time.Millisecond); f.cond.Broadcast() }()
		f.cond.Wait()
	}

	var regs []*structs.ServiceRegistration
	for _, reg := range f.regions[req.Region] {
		if reg.ServiceName == req.ServiceName && reg.Namespace == req.Namespace {
			regs = append(regs, reg.Copy())
		}
	}
	resp.Services = regs
	resp.Discovery = structs.EffectiveDiscovery(regs)
	resp.Index = f.index
	return nil
}

func reg(id, dc, zone string, healthy bool) *structs.ServiceRegistration {
	r := &structs.ServiceRegistration{
		ID: id, ServiceName: "api", Namespace: "default", Datacenter: dc, Zone: zone,
		Address: "10.0.0.1", Port: 80, Ready: true, Health: structs.ServiceHealthPassing,
		Discovery: &structs.ServiceDiscovery{Prefer: "zone", MinLocal: 2, OverflowFraction: 0.2, Failover: []string{"us", "ap"}},
	}
	if !healthy {
		r.Health = structs.ServiceHealthCritical
	}
	return r
}

func ids(regs []*structs.ServiceRegistration) []string {
	out := make([]string, len(regs))
	for i, r := range regs {
		out[i] = r.ID
	}
	return out
}

func TestResolver_TopologyAndFailover(t *testing.T) {
	ci.Parallel(t)

	catalog := NewFakeCatalog()
	cache := NewCache(testlog.HCLogger(t), catalog.RPC, func() string { return "token" })
	cache.waitTime = 100 * time.Millisecond
	defer cache.Stop()

	local := structs.Topology{Region: "eu", Datacenter: "dc1", Zone: "a"}
	r := NewResolver(cache, func() structs.Topology { return local })
	ctx := context.Background()
	q := Query{Namespace: "default", Name: "api"}

	catalog.Set("eu",
		reg("a1", "dc1", "a", true), reg("a2", "dc1", "a", true), reg("b1", "dc1", "b", true),
		reg("c1", "dc2", "c", true), reg("c2", "dc2", "c", true), reg("c3", "dc2", "c", true),
		reg("c4", "dc2", "c", true), reg("c5", "dc2", "c", true))
	catalog.Set("us", reg("us1", "us-dc", "x", true))
	catalog.Set("ap", reg("ap1", "ap-dc", "y", true))

	res, err := r.Resolve(ctx, q)
	must.NoError(t, err)
	must.Eq(t, []string{"a1", "a2"}, ids(res.Instances))
	must.Eq(t, "eu", res.Region)

	resolveIDs := func() []string {
		res, err := r.Resolve(ctx, q)
		must.NoError(t, err)
		return ids(res.Instances)
	}
	waitFor := func(exp []string) {
		must.Wait(t, wait.InitialSuccess(
			wait.BoolFunc(func() bool {
				got := resolveIDs()
				return len(got) == len(exp) && (len(exp) == 0 || got[0] == exp[0] && got[len(got)-1] == exp[len(exp)-1])
			}),
			wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond),
		), must.Sprint(exp))
	}

	// one instance left in the zone: the datacenter joins
	catalog.Set("eu",
		reg("a1", "dc1", "a", true), reg("a2", "dc1", "a", false), reg("b1", "dc1", "b", true),
		reg("c1", "dc2", "c", true))
	waitFor([]string{"a1", "b1"})

	// no healthy instance in the region: the first failover region with
	// healthy instances
	catalog.Set("eu", reg("a1", "dc1", "a", false))
	waitFor([]string{"us1"})
	res, _ = r.Resolve(ctx, q)
	must.Eq(t, "us", res.Region)
	must.True(t, res.Known)

	catalog.Set("us", reg("us1", "us-dc", "x", false))
	waitFor([]string{"ap1"})

	// all registrations gone: the remembered settings still fail over
	catalog.Set("eu")
	waitFor([]string{"ap1"})

	// tags are filtered before the topology selection
	tagged := reg("t1", "dc1", "a", true)
	tagged.Tags = []string{"blue"}
	catalog.Set("eu", tagged, reg("a2", "dc1", "a", true))
	waitFor([]string{"t1", "a2"})
	res, _ = r.Resolve(ctx, Query{Namespace: "default", Name: "api", Tag: "blue"})
	must.Eq(t, []string{"t1"}, ids(res.Instances))

	// lookups of another region are not ordered by topology nor failed over
	res, _ = r.Resolve(ctx, Query{Namespace: "default", Name: "api", Region: "us"})
	must.Len(t, 0, res.Instances)
	must.True(t, res.Known)

	// unknown region
	_, err = r.Resolve(ctx, Query{Namespace: "default", Name: "api", Region: "nowhere"})
	must.ErrorContains(t, err, "No path to region")

	catalog.lock.Lock()
	for _, token := range catalog.tokens {
		must.Eq(t, "token", token)
	}
	catalog.lock.Unlock()
}

func TestCache_IdleEntriesAreDropped(t *testing.T) {
	ci.Parallel(t)

	catalog := NewFakeCatalog()
	catalog.Set("eu", reg("a1", "dc1", "a", true))
	cache := NewCache(testlog.HCLogger(t), catalog.RPC, func() string { return "" })
	cache.waitTime = 50 * time.Millisecond
	cache.idleTimeout = 100 * time.Millisecond

	regs, d, err := cache.Get(context.Background(), Key{Region: "eu", Namespace: "default", Name: "api"})
	must.NoError(t, err)
	must.Len(t, 1, regs)
	must.NotNil(t, d)
	must.Eq(t, 1, cache.Len())

	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return cache.Len() == 0 }),
		wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond),
	))

	cache.Stop()
	_, _, err = cache.Get(context.Background(), Key{Region: "eu", Namespace: "default", Name: "api"})
	must.ErrorIs(t, err, ErrStopped)
}
