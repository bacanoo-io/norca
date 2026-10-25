//go:build integration

package storage

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// replica opens a MySQLBackend standing in for one vault replica. Two replicas
// in a test are two backends — two connection pools — over the same database.
func replica(t *testing.T) *MySQLBackend {
	t.Helper()
	b, err := NewMySQLBackend(mysqlDSN(t))
	if err != nil {
		t.Fatalf("NewMySQLBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// lockName gives each test its own lock row and key prefix.
func lockName(t *testing.T) string {
	return strings.ReplaceAll(t.Name(), "/", "-")
}

func acquireWithin(t *testing.T, l Lock, d time.Duration) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	lost, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return lost
}

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

var fastLock = LockOptions{TTL: time.Second, RenewInterval: 100 * time.Millisecond, RetryInterval: 50 * time.Millisecond}

func TestHALockIsExclusiveAndReleaseHandsOver(t *testing.T) {
	ctx := context.Background()
	name := lockName(t)
	a := replica(t).HALock(name, "a", "https://a:8200", fastLock)
	b := replica(t).HALock(name, "b", "https://b:8200", fastLock)

	acquireWithin(t, a, 5*time.Second)
	if h, err := a.Holder(ctx); err != nil || h.ID != "a" || h.Advertise != "https://a:8200" {
		t.Fatalf("Holder = %+v, %v; want a", h, err)
	}

	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := b.Acquire(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Acquire while held = %v, want DeadlineExceeded", err)
	}

	start := time.Now()
	if err := a.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	acquireWithin(t, b, 5*time.Second)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("handover after Release took %v, want about one retry interval", took)
	}
	h, err := b.Holder(ctx)
	if err != nil || h.ID != "b" {
		t.Fatalf("Holder after handover = %+v, %v; want b", h, err)
	}
}

// TestHALockFencesPausedHolder is the split-brain case the fence exists for: a
// holder stops renewing (a GC pause, a partition) long enough for another
// replica to take over, then resumes and writes as if still active.
func TestHALockFencesPausedHolder(t *testing.T) {
	ctx := context.Background()
	name := lockName(t)
	key := name + "/k"
	ra, rb := replica(t), replica(t)
	// A never renews: its lease runs out in the database while A itself is none
	// the wiser, exactly like a paused process.
	a := ra.HALock(name, "a", "", LockOptions{TTL: 500 * time.Millisecond, RenewInterval: time.Hour, RetryInterval: 50 * time.Millisecond})
	b := rb.HALock(name, "b", "", fastLock)

	lostA := acquireWithin(t, a, 5*time.Second)
	if err := ra.Put(ctx, &Entry{Key: key, Value: []byte("from-a")}); err != nil {
		t.Fatalf("A Put while holding: %v", err)
	}

	acquireWithin(t, b, 5*time.Second) // succeeds once A's lease expires
	if err := rb.Put(ctx, &Entry{Key: key, Value: []byte("from-b")}); err != nil {
		t.Fatalf("B Put after takeover: %v", err)
	}

	// A resumes, believing it is still active: every write is refused.
	if closedWithin(lostA, 0) {
		t.Fatal("paused A noticed the loss; this test needs it not to")
	}
	if err := ra.Put(ctx, &Entry{Key: key, Value: []byte("stale")}); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale A Put = %v, want ErrFenced", err)
	}
	if err := ra.Delete(ctx, key); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale A Delete = %v, want ErrFenced", err)
	}
	got, err := rb.Get(ctx, key)
	if err != nil || got == nil || string(got.Value) != "from-b" {
		t.Fatalf("value = %+v, %v; want from-b", got, err)
	}
	// Reads are never fenced.
	if _, err := ra.Get(ctx, key); err != nil {
		t.Fatalf("stale A Get: %v", err)
	}
}

// TestHALockNoWriteLandsAfterTakeover races a stream of writes from the old
// holder against the takeover: once B's Acquire returns, nothing A writes may
// land, because B's generation bump waits for A's in-flight fenced writes and
// every later one fails the check.
func TestHALockNoWriteLandsAfterTakeover(t *testing.T) {
	ctx := context.Background()
	name := lockName(t)
	key := name + "/k"
	ra, rb := replica(t), replica(t)
	a := ra.HALock(name, "a", "", LockOptions{TTL: 300 * time.Millisecond, RenewInterval: time.Hour, RetryInterval: 20 * time.Millisecond})
	b := rb.HALock(name, "b", "", LockOptions{TTL: time.Second, RenewInterval: 100 * time.Millisecond, RetryInterval: 5 * time.Millisecond})
	acquireWithin(t, a, 5*time.Second)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var fenced atomic.Int64
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				err := ra.Put(ctx, &Entry{Key: key, Value: []byte(strconv.Itoa(w) + "-" + strconv.Itoa(i))})
				if errors.Is(err, ErrFenced) {
					fenced.Add(1)
				} else if err != nil {
					t.Errorf("A Put: %v", err)
					return
				}
			}
		}()
	}

	acquireWithin(t, b, 5*time.Second)
	snap, err := rb.Get(ctx, key)
	if err != nil || snap == nil {
		t.Fatalf("Get after takeover = %+v, %v", snap, err)
	}
	time.Sleep(200 * time.Millisecond) // A keeps trying to write meanwhile
	close(stop)
	wg.Wait()

	after, err := rb.Get(ctx, key)
	if err != nil || after == nil || string(after.Value) != string(snap.Value) {
		t.Fatalf("value changed after takeover: %q -> %+v (%v)", snap.Value, after, err)
	}
	if fenced.Load() == 0 {
		t.Fatal("no write from A was fenced; the race never happened")
	}
}

func TestHALockReleaseFencesAndSignalsLost(t *testing.T) {
	ctx := context.Background()
	name := lockName(t)
	r := replica(t)
	l := r.HALock(name, "a", "", fastLock)

	lost := acquireWithin(t, l, 5*time.Second)
	if err := r.Put(ctx, &Entry{Key: name + "/k", Value: []byte("v")}); err != nil {
		t.Fatalf("Put while holding: %v", err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !closedWithin(lost, time.Second) {
		t.Fatal("lost not closed by Release")
	}
	if err := r.Put(ctx, &Entry{Key: name + "/k", Value: []byte("v2")}); !errors.Is(err, ErrFenced) {
		t.Fatalf("Put after Release = %v, want ErrFenced (a standby must not write)", err)
	}
	if h, err := l.Holder(ctx); err != nil || h.ID != "" {
		t.Fatalf("Holder after Release = %+v, %v; want none", h, err)
	}

	// The replica can become active again later.
	acquireWithin(t, l, 5*time.Second)
	if err := r.Put(ctx, &Entry{Key: name + "/k", Value: []byte("v3")}); err != nil {
		t.Fatalf("Put after reacquire: %v", err)
	}
}

func TestHALockDetectsSupersession(t *testing.T) {
	ctx := context.Background()
	name := lockName(t)
	r := replica(t)
	lost := acquireWithin(t, r.HALock(name, "a", "", fastLock), 5*time.Second)

	// Another replica takes the lock out from under A (as a takeover after A's
	// lease lapsed would): the next renewal finds the row is no longer A's.
	if _, err := replica(t).db.ExecContext(ctx,
		"UPDATE "+mysqlLockTable+" SET holder_id = 'b', generation = generation + 1 WHERE name = ?", name); err != nil {
		t.Fatalf("simulate takeover: %v", err)
	}
	if !closedWithin(lost, time.Second) {
		t.Fatal("holder did not notice it was superseded within a second")
	}
}

func TestHALockLostWhenRenewalsFail(t *testing.T) {
	r := replica(t)
	opts := LockOptions{TTL: 400 * time.Millisecond, RenewInterval: 100 * time.Millisecond, RetryInterval: 50 * time.Millisecond}
	lost := acquireWithin(t, r.HALock(lockName(t), "a", "", opts), 5*time.Second)

	// The database becomes unreachable from this replica.
	_ = r.db.Close()
	if !closedWithin(lost, time.Second) {
		t.Fatal("holder kept the lock with renewals failing past TTL-RenewInterval")
	}
}

// TestHALockHandleFencesBeforeAcquire: a replica that has asked for a lock
// handle but never held the lock — a standby since startup — cannot write.
func TestHALockHandleFencesBeforeAcquire(t *testing.T) {
	ctx := context.Background()
	name := lockName(t)
	r := replica(t)
	l := r.HALock(name, "a", "", fastLock)
	if err := r.Put(ctx, &Entry{Key: name + "/k", Value: []byte("v")}); !errors.Is(err, ErrFenced) {
		t.Fatalf("Put before any acquisition = %v, want ErrFenced", err)
	}
	acquireWithin(t, l, 5*time.Second)
	if err := r.Put(ctx, &Entry{Key: name + "/k", Value: []byte("v")}); err != nil {
		t.Fatalf("Put once held: %v", err)
	}
}
