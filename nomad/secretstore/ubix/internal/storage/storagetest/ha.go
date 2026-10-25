// Package storagetest provides storage test doubles shared across packages.
package storagetest

import (
	"context"
	"sync"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// HAGroup stands in for one database shared by several replicas: the data
// lives in a single inner backend, and one in-memory lock elects the active
// replica. Each [HAGroup.Replica] is one replica's view, with its own fencing,
// matching the MySQL backend's semantics without a database.
type HAGroup struct {
	inner storage.Backend

	mu        sync.Mutex
	holder    string
	advertise string
	gen       uint64
	lost      chan struct{} // closed when the current acquisition ends
}

// NewHAGroup returns a group over inner.
func NewHAGroup(inner storage.Backend) *HAGroup { return &HAGroup{inner: inner} }

// Replica returns a new replica's view of the shared storage.
func (g *HAGroup) Replica() *Replica { return &Replica{g: g} }

// Holder reports the current holder's ID, or "" if none.
func (g *HAGroup) Holder() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holder
}

// Revoke ends the current acquisition as if its lease had lapsed and another
// replica had taken over: the holder's lost channel closes and its writes are
// fenced from now on.
func (g *HAGroup) Revoke() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.endLocked()
	g.gen++
}

func (g *HAGroup) endLocked() {
	if g.holder == "" {
		return
	}
	close(g.lost)
	g.holder, g.advertise, g.lost = "", "", nil
}

// Replica is one replica's [storage.HABackend] over an [HAGroup].
type Replica struct {
	g *HAGroup

	mu     sync.Mutex
	fenced bool
	holder string
	gen    uint64
}

var _ storage.HABackend = (*Replica)(nil)

// Get reads the shared storage; reads are never fenced.
func (r *Replica) Get(ctx context.Context, key string) (*storage.Entry, error) {
	return r.g.inner.Get(ctx, key)
}

// List reads the shared storage; reads are never fenced.
func (r *Replica) List(ctx context.Context, prefix string) ([]string, error) {
	return r.g.inner.List(ctx, prefix)
}

// Put writes the shared storage, fenced like the MySQL backend's writes.
func (r *Replica) Put(ctx context.Context, e *storage.Entry) error {
	return r.write(func() error { return r.g.inner.Put(ctx, e) })
}

// Delete removes from the shared storage, fenced like Put.
func (r *Replica) Delete(ctx context.Context, key string) error {
	return r.write(func() error { return r.g.inner.Delete(ctx, key) })
}

// write runs fn if this replica is unfenced or holds the acquisition it is
// fenced to, holding the group lock so no takeover can interleave.
func (r *Replica) write(fn func() error) error {
	r.mu.Lock()
	fenced, holder, gen := r.fenced, r.holder, r.gen
	r.mu.Unlock()
	if !fenced {
		return fn()
	}
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	if r.g.holder != holder || r.g.gen != gen {
		return storage.ErrFenced
	}
	return fn()
}

// HALock implements [storage.HABackend]; the first handle arms fencing.
func (r *Replica) HALock(_, holderID, advertise string, opts storage.LockOptions) storage.Lock {
	r.mu.Lock()
	if !r.fenced {
		r.fenced, r.holder = true, holderID
	}
	r.mu.Unlock()
	retry := opts.RetryInterval
	if retry <= 0 {
		retry = 5 * time.Millisecond
	}
	return &lock{r: r, id: holderID, advertise: advertise, retry: retry}
}

type lock struct {
	r         *Replica
	id        string
	advertise string
	retry     time.Duration

	mu   sync.Mutex
	gen  uint64 // the acquisition this handle holds, 0 if none
	lost chan struct{}
}

func (l *lock) Acquire(ctx context.Context) (<-chan struct{}, error) {
	g := l.r.g
	for {
		g.mu.Lock()
		l.mu.Lock()
		if l.gen != 0 && g.holder == l.id && g.gen == l.gen {
			lost := l.lost
			l.mu.Unlock()
			g.mu.Unlock()
			return lost, nil
		}
		if g.holder == "" {
			g.gen++
			g.holder, g.advertise, g.lost = l.id, l.advertise, make(chan struct{})
			l.gen, l.lost = g.gen, g.lost
			l.r.mu.Lock()
			l.r.holder, l.r.gen = l.id, g.gen
			l.r.mu.Unlock()
			lost := l.lost
			l.mu.Unlock()
			g.mu.Unlock()
			return lost, nil
		}
		l.mu.Unlock()
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(l.retry):
		}
	}
}

func (l *lock) Release(context.Context) error {
	g := l.r.g
	g.mu.Lock()
	defer g.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gen != 0 && g.holder == l.id && g.gen == l.gen {
		g.endLocked()
	}
	l.gen = 0
	return nil
}

func (l *lock) Holder(context.Context) (storage.Holder, error) {
	g := l.r.g
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.holder == "" {
		return storage.Holder{}, nil
	}
	return storage.Holder{ID: g.holder, Advertise: g.advertise, Generation: g.gen}, nil
}
