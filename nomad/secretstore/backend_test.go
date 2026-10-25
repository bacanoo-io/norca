// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore/ubix/export/barrier"
	"github.com/hashicorp/nomad/secretstore/ubix/export/core"
	"github.com/hashicorp/nomad/secretstore/ubix/export/kv"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeRaft stands in for the server: it applies the backend's requests to a
// state store the way the FSM does, on the leader only.
type fakeRaft struct {
	mu      sync.Mutex
	store   *state.StateStore
	index   uint64
	leader  bool
	term    uint64
	ready   atomic.Bool
	applied []structs.MessageType
}

func newFakeRaft(t *testing.T) *fakeRaft {
	r := &fakeRaft{store: state.TestStateStore(t), term: 1}
	r.ready.Store(true)
	return r
}

var errNotLeader = errors.New("node is not the leader")

func (r *fakeRaft) apply(msgType structs.MessageType, req any) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.leader {
		return 0, errNotLeader
	}
	r.index++
	r.applied = append(r.applied, msgType)
	// Round-trip through the Raft encoding, as a real log entry does.
	buf, err := structs.Encode(msgType, req)
	if err != nil {
		return 0, err
	}
	t := structs.MessageType(buf[0]) &^ structs.IgnoreUnknownTypeFlag
	switch t {
	case structs.SecretStoreBlobsUpsertRequestType:
		var r2 structs.SecretStoreBlobsUpsertRequest
		if err := structs.Decode(buf[1:], &r2); err != nil {
			return 0, err
		}
		return r.index, r.store.UpsertSecretStoreBlobs(t, r.index, r2.Blobs)
	case structs.SecretStoreBlobsDeleteRequestType:
		var r2 structs.SecretStoreBlobsDeleteRequest
		if err := structs.Decode(buf[1:], &r2); err != nil {
			return 0, err
		}
		return r.index, r.store.DeleteSecretStoreBlobs(t, r.index, r2.Paths)
	}
	return 0, fmt.Errorf("unexpected message type %d", t)
}

func (r *fakeRaft) setLeader(b *Backend, leader bool) {
	r.mu.Lock()
	r.leader = leader
	if leader {
		r.term++
	}
	r.mu.Unlock()
	b.SetLeader(leader)
}

func (r *fakeRaft) currentTerm() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.term
}

func newTestBackend(t *testing.T) (*Backend, *fakeRaft) {
	r := newFakeRaft(t)
	b := New(&Deps{
		State:        func() *state.StateStore { return r.store },
		Apply:        r.apply,
		ServersReady: r.ready.Load,
		Leader:       func() (string, string) { return "server-2", "10.0.0.2:4647" },
		Term:         r.currentTerm,
	})
	return b, r
}

func TestBackend_Storage(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	b, r := newTestBackend(t)
	r.setLeader(b, true)

	e, err := b.Get(ctx, "a")
	must.NoError(t, err)
	must.Nil(t, e)

	for _, k := range []string{"a", "b/c", "b/d", "e/f/g", "b-x"} {
		must.NoError(t, b.Put(ctx, &storage.Entry{Key: k, Value: []byte("v:" + k)}))
	}
	// Writes go through Raft, flagged as safe to ignore by older servers.
	must.Eq(t, structs.SecretStoreBlobsUpsertRequestType|structs.IgnoreUnknownTypeFlag, r.applied[0])

	e, err = b.Get(ctx, "b/c")
	must.NoError(t, err)
	must.Eq(t, &storage.Entry{Key: "b/c", Value: []byte("v:b/c")}, e)

	// Get returns a copy.
	e.Value[0] = 'X'
	e, err = b.Get(ctx, "b/c")
	must.NoError(t, err)
	must.Eq(t, []byte("v:b/c"), e.Value)

	// The documented List semantics of the storage interface.
	for prefix, want := range map[string][]string{
		"":       {"a", "b-x", "b/", "e/"},
		"b/":     {"c", "d"},
		"e/":     {"f/"},
		"e/f/":   {"g"},
		"none/":  {},
		"b/c/":   {},
		"a/":     {},
		"e/f/g/": {},
	} {
		got, err := b.List(ctx, prefix)
		must.NoError(t, err, must.Sprintf("prefix %q", prefix))
		must.Eq(t, want, got, must.Sprintf("prefix %q", prefix))
	}

	// Overwrite and delete; deleting a missing key is a no-op.
	must.NoError(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("new")}))
	e, err = b.Get(ctx, "a")
	must.NoError(t, err)
	must.Eq(t, []byte("new"), e.Value)
	must.NoError(t, b.Delete(ctx, "a"))
	must.NoError(t, b.Delete(ctx, "a"))
	e, err = b.Get(ctx, "a")
	must.NoError(t, err)
	must.Nil(t, e)
	must.Eq(t, structs.SecretStoreBlobsDeleteRequestType|structs.IgnoreUnknownTypeFlag, r.applied[len(r.applied)-1])

	// An empty value is a value.
	must.NoError(t, b.Put(ctx, &storage.Entry{Key: "empty"}))
	e, err = b.Get(ctx, "empty")
	must.NoError(t, err)
	must.NotNil(t, e)
	must.SliceEmpty(t, e.Value)
}

func TestBackend_Validation(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	b, r := newTestBackend(t)
	r.setLeader(b, true)

	for _, k := range []string{"", "/a", "a/", "a//b", "a/./b", "../a"} {
		_, err := b.Get(ctx, k)
		must.ErrorIs(t, err, storage.ErrInvalidKey, must.Sprintf("key %q", k))
		must.ErrorIs(t, b.Put(ctx, &storage.Entry{Key: k}), storage.ErrInvalidKey)
		must.ErrorIs(t, b.Delete(ctx, k), storage.ErrInvalidKey)
	}
	for _, p := range []string{"a", "/", "/a/", "a//"} {
		_, err := b.List(ctx, p)
		must.ErrorIs(t, err, storage.ErrInvalidKey, must.Sprintf("prefix %q", p))
	}
	must.Error(t, b.Put(ctx, nil))

	// The size limit.
	big := &storage.Entry{Key: "big", Value: make([]byte, DefaultMaxValueSize+1)}
	must.ErrorIs(t, b.Put(ctx, big), ErrValueTooLarge)
	big.Value = big.Value[:DefaultMaxValueSize]
	must.NoError(t, b.Put(ctx, big))

	// A canceled context stops the request.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	must.ErrorIs(t, b.Put(cctx, &storage.Entry{Key: "x"}), context.Canceled)
	_, err := b.Get(cctx, "x")
	must.ErrorIs(t, err, context.Canceled)
}

func TestBackend_VersionGate(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	b, r := newTestBackend(t)
	r.setLeader(b, true)

	r.ready.Store(false)
	must.ErrorIs(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("v")}), ErrServersNotReady)
	must.ErrorIs(t, b.Delete(ctx, "a"), ErrServersNotReady)
	must.SliceEmpty(t, r.applied)

	r.ready.Store(true)
	must.NoError(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("v")}))
}

func TestBackend_FollowerWrites(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	b, _ := newTestBackend(t)

	// Without HA, a follower's write is refused by Raft.
	must.ErrorIs(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("v")}), errNotLeader)
}

func TestBackend_HALock(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	b, r := newTestBackend(t)
	r.setLeader(b, true)

	// Unfenced while no lock handle exists.
	must.NoError(t, b.Put(ctx, &storage.Entry{Key: "before", Value: []byte("v")}))

	lock := b.HALock("active", "server-1", "advert-1", storage.LockOptions{})
	// The first handle fences writes: none is held yet.
	must.ErrorIs(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("v")}), storage.ErrFenced)
	must.ErrorIs(t, b.Delete(ctx, "before"), storage.ErrFenced)

	// The holder of a leader without an active lock is nobody.
	h, err := lock.Holder(ctx)
	must.NoError(t, err)
	must.Eq(t, storage.Holder{}, h)

	lost, err := lock.Acquire(ctx)
	must.NoError(t, err)
	must.NoError(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("v")}))
	h, err = lock.Holder(ctx)
	must.NoError(t, err)
	must.Eq(t, storage.Holder{ID: "server-1", Advertise: "advert-1", Generation: 2}, h)

	// Acquire again while held returns the same acquisition.
	lost2, err := lock.Acquire(ctx)
	must.NoError(t, err)
	must.Eq(t, lost, lost2)

	// A second handle waits while the first holds the lock.
	other := b.HALock("active", "server-1/init", "advert-1", storage.LockOptions{})
	actx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = other.Acquire(actx)
	cancel()
	must.ErrorIs(t, err, context.DeadlineExceeded)

	// Release ends the acquisition and fences writes; the other handle gets
	// the lock.
	must.NoError(t, lock.Release(ctx))
	assertClosed(t, lost)
	must.ErrorIs(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("v")}), storage.ErrFenced)
	otherLost, err := other.Acquire(ctx)
	must.NoError(t, err)
	must.NoError(t, other.Release(ctx))
	assertClosed(t, otherLost)
	must.NoError(t, lock.Release(ctx)) // releasing a released handle is a no-op

	// Losing leadership ends the acquisition.
	lost, err = lock.Acquire(ctx)
	must.NoError(t, err)
	r.setLeader(b, false)
	assertClosed(t, lost)
	must.ErrorIs(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("v")}), storage.ErrFenced)

	// A follower reports the Raft leader as the holder.
	h, err = lock.Holder(ctx)
	must.NoError(t, err)
	must.Eq(t, storage.Holder{ID: "server-2", Advertise: "10.0.0.2:4647", Generation: 2}, h)

	// A follower waits in Acquire until it leads again.
	acquired := make(chan (<-chan struct{}), 1)
	go func() {
		l, err := lock.Acquire(ctx)
		if err == nil {
			acquired <- l
		}
	}()
	select {
	case <-acquired:
		t.Fatal("acquired the lock as a follower")
	case <-time.After(50 * time.Millisecond):
	}
	r.setLeader(b, true)
	select {
	case lost = <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("lock not acquired after becoming leader")
	}
	h, err = lock.Holder(ctx)
	must.NoError(t, err)
	must.Eq(t, 3, h.Generation)
	must.NoError(t, b.Put(ctx, &storage.Entry{Key: "a", Value: []byte("v")}))

	// SetLeader is idempotent and nil-safe.
	b.SetLeader(true)
	assertOpen(t, lost)
	var nilBackend *Backend
	nilBackend.SetLeader(true)
}

func assertClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed")
	}
}

func assertOpen(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("channel closed")
	default:
	}
}

// TestBackend_Barrier runs uBixVault's barrier and KV engine on the backend:
// every value reaches the state store encrypted.
func TestBackend_Barrier(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	b, r := newTestBackend(t)
	r.setLeader(b, true)

	master := bytes.Repeat([]byte{7}, barrier.KeySize)
	bar := barrier.New(b)
	must.NoError(t, bar.Initialize(ctx, master))
	must.NoError(t, bar.Unseal(ctx, master))

	secrets := kv.New(bar, "secret")
	_, err := secrets.Write(ctx, "app/db", map[string]any{"password": "canary-0815"})
	must.NoError(t, err)
	data, meta, err := secrets.Read(ctx, "app/db", 0)
	must.NoError(t, err)
	must.Eq(t, "canary-0815", data["password"])
	must.Eq(t, 1, meta.Version)

	assertNoPlaintext(t, r.store, "canary-0815")

	bar.Seal()
	_, _, err = secrets.Read(ctx, "app/db", 0)
	must.Error(t, err)
	must.NoError(t, bar.Unseal(ctx, master))
	data, _, err = secrets.Read(ctx, "app/db", 0)
	must.NoError(t, err)
	must.Eq(t, "canary-0815", data["password"])
}

// TestBackend_CoreHA runs uBixVault's core with HA on the backend: it becomes
// active only while the server leads, and its writes are fenced after it
// loses leadership.
func TestBackend_CoreHA(t *testing.T) {
	ci.Parallel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, r := newTestBackend(t)
	r.setLeader(b, true)

	c := core.New(b, core.WithHA(core.HAConfig{Backend: b, HolderID: "server-1"}))
	init, err := c.Initialize(ctx, core.InitConfig{SecretShares: 3, SecretThreshold: 2})
	must.NoError(t, err)
	must.Len(t, 3, init.Keys)

	done := make(chan error, 1)
	go func() { done <- c.RunHA(ctx, t.Logf) }()

	for _, share := range init.Keys[:2] {
		_, err := c.Unseal(ctx, share)
		must.NoError(t, err)
	}
	waitFor(t, "active", c.Active)

	must.NoError(t, c.Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("canary-4711")}))
	assertNoPlaintext(t, r.store, "canary-4711")

	// Leadership lost: the core steps down, its writes are fenced, and reads
	// still work.
	r.setLeader(b, false)
	waitFor(t, "standby", c.Standby)
	must.ErrorIs(t, c.Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("x")}), storage.ErrFenced)
	e, err := c.Barrier().Get(ctx, "secret/app")
	must.NoError(t, err)
	must.Eq(t, []byte("canary-4711"), e.Value)
	info, err := c.Leader(ctx)
	must.NoError(t, err)
	must.Eq(t, core.LeaderInfo{HolderID: "server-2", APIAddr: "10.0.0.2:4647"}, info)

	// Leadership regained: active again.
	r.setLeader(b, true)
	waitFor(t, "active again", c.Active)
	must.NoError(t, c.Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("canary-4712")}))
	info, err = c.Leader(ctx)
	must.NoError(t, err)
	must.True(t, info.IsSelf)

	cancel()
	select {
	case err := <-done:
		must.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("RunHA did not return")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(cond),
		wait.Timeout(10*time.Second),
		wait.Gap(10*time.Millisecond),
	), must.Sprintf("waiting for %s", what))
}

// assertNoPlaintext fails if a stored blob contains the canary in the clear.
func assertNoPlaintext(t *testing.T, store *state.StateStore, canary string) {
	t.Helper()
	iter, err := store.SecretStoreBlobsByPrefix(nil, "")
	must.NoError(t, err)
	n := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		blob := raw.(*structs.SecretStoreBlob)
		must.False(t, bytes.Contains(blob.Value, []byte(canary)), must.Sprintf("plaintext in %s", blob.Path))
		n++
	}
	must.Positive(t, n)
}
