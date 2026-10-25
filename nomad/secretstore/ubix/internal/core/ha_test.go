package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/storage/storagetest"
)

// haReplica is one replica in a test cluster.
type haReplica struct {
	core     *Core
	id       string
	actives  atomic.Int32 // OnActive calls
	standbys atomic.Int32 // OnStandby calls
	stop     context.CancelFunc
	done     chan struct{}
}

// haCluster builds n auto-unseal replicas sharing one in-memory database, each
// running its election loop until the test ends.
func haCluster(t *testing.T, n int, opts ...Option) (*storagetest.HAGroup, []*haReplica) {
	t.Helper()
	group := storagetest.NewHAGroup(storage.NewMemoryBackend())
	var rs []*haReplica
	for i := range n {
		phys := group.Replica()
		r := &haReplica{id: fmt.Sprintf("r%d", i), done: make(chan struct{})}
		r.core = New(phys, append([]Option{WithHA(HAConfig{
			Backend: phys, HolderID: r.id, Advertise: "https://" + r.id + ":8200",
			Lock: storage.LockOptions{RetryInterval: 20 * time.Millisecond},
		})}, opts...)...)
		r.core.OnActive(func() { r.actives.Add(1) })
		r.core.OnStandby(func() { r.standbys.Add(1) })
		ctx, cancel := context.WithCancel(context.Background())
		r.stop = cancel
		go func() {
			defer close(r.done)
			_ = r.core.RunHA(ctx, func(string, ...any) {})
		}()
		t.Cleanup(func() { cancel(); <-r.done })
		rs = append(rs, r)
	}
	return group, rs
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func activeReplicas(rs []*haReplica) []*haReplica {
	var out []*haReplica
	for _, r := range rs {
		if r.core.Active() {
			out = append(out, r)
		}
	}
	return out
}

// initAndUnsealAll initializes the cluster through the first replica and
// auto-unseals the rest.
func initAndUnsealAll(t *testing.T, rs []*haReplica) {
	t.Helper()
	ctx := context.Background()
	if _, err := rs[0].core.Initialize(ctx, InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	for _, r := range rs[1:] {
		if err := r.core.AutoUnseal(ctx); err != nil {
			t.Fatalf("AutoUnseal %s: %v", r.id, err)
		}
	}
}

func TestHAExactlyOneActive(t *testing.T) {
	kek := newKEK(t)
	group, rs := haCluster(t, 3, WithAutoUnsealKey(kek))
	initAndUnsealAll(t, rs)

	eventually(t, "one active replica", func() bool { return len(activeReplicas(rs)) == 1 })
	active := activeReplicas(rs)[0]
	for _, r := range rs {
		if r != active && !r.core.Standby() {
			t.Fatalf("%s is neither active nor standby", r.id)
		}
	}
	if group.Holder() != active.id {
		t.Fatalf("lock holder = %q, active replica = %q", group.Holder(), active.id)
	}
	for _, r := range rs {
		h, err := r.core.Leader(context.Background())
		if err != nil || h.HolderID != active.id || h.APIAddr != "https://"+active.id+":8200" || h.IsSelf != (r == active) {
			t.Fatalf("Leader seen from %s = %+v, %v; want %s (self: %v)", r.id, h, err, active.id, r == active)
		}
	}
	// Stays that way: no flapping.
	time.Sleep(50 * time.Millisecond)
	if got := activeReplicas(rs); len(got) != 1 || got[0] != active {
		t.Fatalf("active set changed with nothing happening: %v", got)
	}
}

func TestHAStandbyCannotWrite(t *testing.T) {
	_, rs := haCluster(t, 2, WithAutoUnsealKey(newKEK(t)))
	initAndUnsealAll(t, rs)
	eventually(t, "one active replica", func() bool { return len(activeReplicas(rs)) == 1 })

	for _, r := range rs {
		err := r.core.Barrier().Put(context.Background(), &storage.Entry{Key: "secret/x", Value: []byte("v")})
		if r.core.Active() && err != nil {
			t.Fatalf("active %s Put: %v", r.id, err)
		}
		if r.core.Standby() && !errors.Is(err, storage.ErrFenced) {
			t.Fatalf("standby %s Put = %v, want ErrFenced", r.id, err)
		}
	}
}

func TestHASealingActiveFailsOver(t *testing.T) {
	_, rs := haCluster(t, 2, WithAutoUnsealKey(newKEK(t)))
	initAndUnsealAll(t, rs)
	eventually(t, "one active replica", func() bool { return len(activeReplicas(rs)) == 1 })
	old := activeReplicas(rs)[0]

	old.core.Seal()
	eventually(t, "the other replica to take over", func() bool {
		a := activeReplicas(rs)
		return len(a) == 1 && a[0] != old
	})
	if old.core.Active() || old.core.Standby() {
		t.Fatal("sealed replica reports active or standby")
	}
	eventually(t, "standby hook on the sealed replica", func() bool { return old.standbys.Load() == 1 })
}

func TestHALostLockStepsDownAndRecovers(t *testing.T) {
	group, rs := haCluster(t, 2, WithAutoUnsealKey(newKEK(t)))
	initAndUnsealAll(t, rs)
	eventually(t, "one active replica", func() bool { return len(activeReplicas(rs)) == 1 })
	old := activeReplicas(rs)[0]
	before := old.standbys.Load()

	group.Revoke() // the lease lapsed out from under it
	eventually(t, "old active to step down", func() bool { return old.standbys.Load() == before+1 })
	eventually(t, "an active replica again", func() bool { return len(activeReplicas(rs)) == 1 })
}

func TestHAShutdownHandsOver(t *testing.T) {
	_, rs := haCluster(t, 2, WithAutoUnsealKey(newKEK(t)))
	initAndUnsealAll(t, rs)
	eventually(t, "one active replica", func() bool { return len(activeReplicas(rs)) == 1 })
	old := activeReplicas(rs)[0]

	old.stop()
	<-old.done // RunHA returns only after stepping down and releasing
	if old.core.Active() {
		t.Fatal("replica still active after RunHA returned")
	}
	eventually(t, "the other replica to take over", func() bool {
		a := activeReplicas(rs)
		return len(a) == 1 && a[0] != old
	})
}

func TestHAConcurrentInitializeOnce(t *testing.T) {
	_, rs := haCluster(t, 2) // Shamir
	var wg sync.WaitGroup
	errs := make([]error, len(rs))
	for i, r := range rs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.core.Initialize(context.Background(), InitConfig{SecretShares: 3, SecretThreshold: 2})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrAlreadyInitialized):
			t.Fatalf("Initialize: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d replicas initialized the same database, want 1", ok)
	}
}

func TestActiveWithoutHA(t *testing.T) {
	c := New(storage.NewMemoryBackend(), WithAutoUnsealKey(newKEK(t)))
	if c.Active() || c.Standby() || c.HAEnabled() {
		t.Fatal("sealed non-HA core reports active, standby, or HA")
	}
	if _, err := c.Initialize(context.Background(), InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !c.Active() || c.Standby() {
		t.Fatal("unsealed non-HA core should be active and never standby")
	}
	if err := c.RunHA(context.Background(), nil); !errors.Is(err, ErrHANotConfigured) {
		t.Fatalf("RunHA without HA = %v, want ErrHANotConfigured", err)
	}
}

func TestHAStepDownHandsOverAndHoldsOff(t *testing.T) {
	_, rs := haCluster(t, 2, WithAutoUnsealKey(newKEK(t)))
	initAndUnsealAll(t, rs)
	eventually(t, "one active replica", func() bool { return len(activeReplicas(rs)) == 1 })
	old := activeReplicas(rs)[0]
	var other *haReplica
	for _, r := range rs {
		if r != old {
			other = r
		}
	}
	if err := other.core.StepDown(); !errors.Is(err, ErrNotActive) {
		t.Fatalf("StepDown on a standby = %v, want ErrNotActive", err)
	}
	if err := old.core.StepDown(); err != nil {
		t.Fatalf("StepDown: %v", err)
	}
	// The stepped-down replica holds off, so the other one — not it — wins.
	eventually(t, "the other replica to take over", func() bool { return other.core.Active() })
	if old.core.Active() {
		t.Fatal("stepped-down replica won the lock straight back")
	}
	// Both have now stepped down. Holding off must not leave the vault without
	// an active replica: once the lock sits free, one of them takes it.
	if err := other.core.StepDown(); err != nil {
		t.Fatalf("second StepDown: %v", err)
	}
	eventually(t, "an active replica despite both holding off", func() bool { return len(activeReplicas(rs)) == 1 })
}

func TestHALeaderReportsClusterAddrAndSelf(t *testing.T) {
	group := storagetest.NewHAGroup(storage.NewMemoryBackend())
	phys := group.Replica()
	c := New(phys, WithAutoUnsealKey(newKEK(t)), WithHA(HAConfig{
		Backend: phys, HolderID: "solo", Advertise: "https://solo:8200", ClusterAddr: "https://solo:8201",
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.RunHA(ctx, func(string, ...any) {}) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, err := c.Initialize(context.Background(), InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	eventually(t, "active", c.Active)
	info, err := c.Leader(context.Background())
	if err != nil || !info.IsSelf || info.APIAddr != "https://solo:8200" || info.ClusterAddr != "https://solo:8201" {
		t.Fatalf("Leader = %+v, %v", info, err)
	}
	if c.ActiveSince().IsZero() {
		t.Fatal("ActiveSince is zero on the active replica")
	}
}

// TestHAHoldOffEndsWhenNobodyElseIsActive: a replica holding off after a
// step-down must not leave the vault without an active replica — if the one
// that took over goes away, it takes the lock back without waiting out the
// hold-off.
func TestHAHoldOffEndsWhenNobodyElseIsActive(t *testing.T) {
	_, rs := haCluster(t, 2, WithAutoUnsealKey(newKEK(t)))
	initAndUnsealAll(t, rs)
	eventually(t, "one active replica", func() bool { return len(activeReplicas(rs)) == 1 })
	old := activeReplicas(rs)[0]
	var other *haReplica
	for _, r := range rs {
		if r != old {
			other = r
		}
	}
	if err := old.core.StepDown(); err != nil {
		t.Fatalf("StepDown: %v", err)
	}
	eventually(t, "the other replica to take over", func() bool { return other.core.Active() })

	start := time.Now()
	other.stop() // the new active shuts down during old's 10s hold-off
	<-other.done
	eventually(t, "the held-off replica to take over", func() bool { return old.core.Active() })
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %v to recover; the hold-off should end once the lock stays free", took)
	}
}
