// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
)

// DefaultMaxValueSize is the default limit of a single stored value. A
// secret of the built-in store is at most 64 KiB; the limit leaves room for
// its versions' metadata and the barrier's envelope while keeping single
// Raft entries small.
const DefaultMaxValueSize = 512 * 1024

var (
	// ErrServersNotReady is returned by writes until every server of the
	// region runs a Norca version that knows the secret store's Raft
	// message types.
	ErrServersNotReady = errors.New("secret store: not all servers in the region support the secret store yet")

	// ErrValueTooLarge is returned by Put for a value above the size limit.
	ErrValueTooLarge = errors.New("secret store: value too large")
)

// Deps are the parts of the Nomad server the backend uses.
type Deps struct {
	Logger hclog.Logger

	// State returns the current state store. It is called on every read,
	// because a snapshot restore replaces the state store.
	State func() *state.StateStore

	// Apply submits a Raft log entry and returns its index once the local
	// FSM has applied it. It fails on a server that is not the leader.
	Apply func(msgType structs.MessageType, req any) (uint64, error)

	// ServersReady reports whether every server of the region supports the
	// secret store's Raft message types (ServersMeetMinimumNorcaVersion).
	ServersReady func() bool

	// Leader returns the server ID and the RPC address of the current Raft
	// leader, empty if there is none.
	Leader func() (id, addr string)

	// Term returns the current Raft term.
	Term func() uint64

	// MaxValueSize limits a stored value; DefaultMaxValueSize if zero.
	MaxValueSize int
}

// Backend is the physical storage and HA lock of the built-in secret store
// on Norca's Raft (see the package documentation). It implements uBixVault's
// storage.HABackend. It is safe for concurrent use.
type Backend struct {
	deps   Deps
	logger hclog.Logger

	mu sync.Mutex
	// leader is true between SetLeader(true) and SetLeader(false).
	leader bool
	// term is the Raft term of the current leadership; 0 when not leader.
	term uint64
	// fenced is set by the first HALock call: from then on writes succeed
	// only while a lock handle of this backend is held.
	fenced bool
	// holder is the lock handle holding the lock, or nil.
	holder *raftLock
	// changed is closed and replaced on every change of leader or holder.
	changed chan struct{}
}

var _ storage.HABackend = (*Backend)(nil)

// New returns a backend over the server dependencies. The backend starts as
// a follower; the server reports leadership with SetLeader.
func New(deps *Deps) *Backend {
	d := *deps
	if d.Logger == nil {
		d.Logger = hclog.NewNullLogger()
	}
	if d.MaxValueSize <= 0 {
		d.MaxValueSize = DefaultMaxValueSize
	}
	return &Backend{
		deps:    d,
		logger:  d.Logger,
		changed: make(chan struct{}),
	}
}

// Get implements storage.Backend. It reads the local state store and returns
// a copy of the stored value, or (nil, nil) if there is none.
func (b *Backend) Get(ctx context.Context, key string) (*storage.Entry, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e, ok := getBatched(ctx, key); ok {
		return e, nil
	}
	blob, err := b.deps.State().SecretStoreBlobByPath(nil, key)
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	if blob == nil {
		return nil, nil
	}
	return &storage.Entry{Key: key, Value: append([]byte(nil), blob.Value...)}, nil
}

// List implements storage.Backend: it returns the immediate children of
// prefix, leaves by their last path segment and subtrees as "segment/",
// sorted and without duplicates.
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if !strings.HasSuffix(prefix, "/") {
			return nil, storage.ErrInvalidKey
		}
		if err := storage.ValidateKey(strings.TrimSuffix(prefix, "/")); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	iter, err := b.deps.State().SecretStoreBlobsByPrefix(memdb.NewWatchSet(), prefix)
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	out := []string{}
	last := ""
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		rest := strings.TrimPrefix(raw.(*structs.SecretStoreBlob).Path, prefix)
		if rest == "" {
			continue
		}
		child := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			child = rest[:i+1]
		}
		// The iterator is ordered by path, so the paths below one subtree
		// are adjacent and its child is added once.
		if child == last {
			continue
		}
		last = child
		out = append(out, child)
	}
	sort.Strings(out)
	return dedupSorted(out), nil
}

// Put implements storage.Backend. The value is written through Raft; Put
// returns once the local FSM has applied it, so a following Get on this
// server reads it.
func (b *Backend) Put(ctx context.Context, entry *storage.Entry) error {
	if entry == nil {
		return errors.New("secret store: nil entry")
	}
	if err := storage.ValidateKey(entry.Key); err != nil {
		return err
	}
	if len(entry.Value) > b.deps.MaxValueSize {
		return fmt.Errorf("%w: %q is %d bytes, the limit is %d", ErrValueTooLarge, entry.Key, len(entry.Value), b.deps.MaxValueSize)
	}
	if batch := batchFrom(ctx); batch != nil {
		batch.put(entry.Key, entry.Value)
		return nil
	}
	if err := b.checkWrite(ctx); err != nil {
		return err
	}
	req := &structs.SecretStoreBlobsUpsertRequest{
		Blobs: []*structs.SecretStoreBlob{{Path: entry.Key, Value: append([]byte(nil), entry.Value...)}},
	}
	if _, err := b.deps.Apply(structs.SecretStoreBlobsUpsertRequestType|structs.IgnoreUnknownTypeFlag, req); err != nil {
		return fmt.Errorf("secret store: write %q: %w", entry.Key, err)
	}
	return nil
}

// Delete implements storage.Backend. Deleting a missing key is a no-op.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	if batch := batchFrom(ctx); batch != nil {
		batch.delete(key)
		return nil
	}
	if err := b.checkWrite(ctx); err != nil {
		return err
	}
	req := &structs.SecretStoreBlobsDeleteRequest{Paths: []string{key}}
	if _, err := b.deps.Apply(structs.SecretStoreBlobsDeleteRequestType|structs.IgnoreUnknownTypeFlag, req); err != nil {
		return fmt.Errorf("secret store: delete %q: %w", key, err)
	}
	return nil
}

// checkWrite applies the version gate and the HA fencing to a write.
func (b *Backend) checkWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.deps.ServersReady() {
		return ErrServersNotReady
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fenced && b.holder == nil {
		return storage.ErrFenced
	}
	return nil
}

// SetLeader reports a change of Nomad leadership. The server calls it with
// true once it has established leadership (its state store is caught up with
// the Raft log) and with false when leadership is revoked; losing leadership
// ends the lock acquisition held on this server. It never blocks.
func (b *Backend) SetLeader(leader bool) {
	if b == nil {
		return
	}
	var term uint64
	if leader {
		term = b.deps.Term()
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.leader == leader && b.term == term {
		return
	}
	if b.holder != nil {
		b.endLocked()
	}
	b.leader, b.term = leader, term
	b.notifyLocked()
}

// HALock implements storage.HABackend. Nomad leadership is the only lock, so
// name is ignored; holderID must be the server's Raft ID, which followers
// report as the holder. The first call fences the backend's writes: they
// succeed only while one of its handles holds the lock. opts are ignored:
// there is no lease to renew, leadership is ended by Raft.
func (b *Backend) HALock(_, holderID, advertise string, _ storage.LockOptions) storage.Lock {
	b.mu.Lock()
	b.fenced = true
	b.mu.Unlock()
	return &raftLock{b: b, id: holderID, advertise: advertise}
}

// endLocked ends the current acquisition. The caller holds b.mu.
func (b *Backend) endLocked() {
	close(b.holder.lost)
	b.holder.lost = nil
	b.holder = nil
	b.notifyLocked()
}

// notifyLocked wakes the waiters in Acquire. The caller holds b.mu.
func (b *Backend) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// raftLock is one lock handle. On the leader the handles of one backend
// exclude each other, as handles on a shared lock do.
type raftLock struct {
	b         *Backend
	id        string
	advertise string

	// lost is closed when the acquisition ends; set while held, guarded
	// by b.mu.
	lost chan struct{}
}

// Acquire implements storage.Lock: it blocks until this server is the
// established leader and no other handle holds the lock, or ctx ends. The
// returned channel is closed when leadership is lost or the lock released.
func (l *raftLock) Acquire(ctx context.Context) (<-chan struct{}, error) {
	b := l.b
	for {
		b.mu.Lock()
		if b.leader && (b.holder == nil || b.holder == l) {
			if b.holder != l {
				b.holder = l
				l.lost = make(chan struct{})
				b.notifyLocked()
				b.logger.Debug("secret store HA lock acquired", "holder", l.id, "term", b.term)
			}
			lost := l.lost
			b.mu.Unlock()
			return lost, nil
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// Release implements storage.Lock. Leadership itself is not given up: the
// handle that released can acquire again at once while this server leads.
func (l *raftLock) Release(context.Context) error {
	b := l.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holder == l {
		b.endLocked()
		b.logger.Debug("secret store HA lock released", "holder", l.id)
	}
	return nil
}

// Holder implements storage.Lock. On the leader it reports the handle holding
// the lock (none while no core is active); on a follower it reports the Raft
// leader by server ID and RPC address. Generation is the Raft term of the
// leadership, so it increases with every change of leader.
func (l *raftLock) Holder(context.Context) (storage.Holder, error) {
	b := l.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holder != nil {
		return storage.Holder{ID: b.holder.id, Advertise: b.holder.advertise, Generation: b.term}, nil
	}
	if b.leader {
		return storage.Holder{}, nil
	}
	id, addr := b.deps.Leader()
	if id == "" {
		return storage.Holder{}, nil
	}
	return storage.Holder{ID: id, Advertise: addr, Generation: b.deps.Term()}, nil
}

// dedupSorted removes adjacent duplicates from a sorted slice.
func dedupSorted(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
