package storage

import (
	"context"
	"errors"
	"time"
)

// ErrFenced is returned by a write from a replica that no longer holds the HA
// lock it acquired — it released the lock, or another replica took it over. It
// is permanent: retrying cannot succeed, and the replica must step down.
var ErrFenced = errors.New("storage: write fenced: this replica does not hold the HA lock")

// HABackend is a [Backend] that can elect a single active writer among the
// replicas sharing it (ADR D-021, docs/design/ha-active-standby.md).
//
// Once a replica has taken a [Lock] handle from its HABackend, every write that
// replica makes through the backend is fenced: it succeeds only while the
// replica holds the lock, and only for the acquisition it currently holds. A replica that was paused past its lease
// and resumes still believing it is active therefore cannot write — its writes
// fail with [ErrFenced] instead of interleaving with the new active replica's.
//
// Core sees only this interface, never the mechanism behind it, so another
// backend (integrated Raft storage, say) can provide the same election.
type HABackend interface {
	Backend
	// HALock returns a handle on the named lock for this replica, identified to
	// other replicas by holderID and advertising advertise (the address standbys
	// forward requests to). A backend has at most one lock handle in use.
	HALock(name, holderID, advertise string, opts LockOptions) Lock
}

// Lock is one replica's handle on an HA lock.
type Lock interface {
	// Acquire blocks until this replica holds the lock or ctx ends. The returned
	// channel is closed when the lock is lost afterwards: superseded by another
	// replica, not renewed in time, or released.
	Acquire(ctx context.Context) (lost <-chan struct{}, err error)
	// Release gives the lock up, if held, so a standby can take over without
	// waiting for the lease to expire. Writes are fenced from then on.
	Release(ctx context.Context) error
	// Holder reports the current holder, or a zero Holder if none.
	Holder(ctx context.Context) (Holder, error)
}

// Holder describes the replica holding an HA lock.
type Holder struct {
	ID         string
	Advertise  string
	Generation uint64 // increases with every acquisition; the fencing token
}

// LockOptions tunes an HA lock's timing. Zero fields take the defaults.
type LockOptions struct {
	// TTL is the lease a holder must renew. An unplanned failover (a crashed or
	// partitioned holder) takes up to TTL plus RetryInterval.
	TTL time.Duration
	// RenewInterval is how often the holder renews. The holder treats the lock
	// as lost once TTL-RenewInterval passes without a successful renewal, so it
	// stops acting as active before its lease can run out.
	RenewInterval time.Duration
	// RetryInterval is how often a replica waiting in Acquire tries again. A
	// planned handoff (Release on shutdown) takes up to this long.
	RetryInterval time.Duration
}

// Default HA lock timing (docs/design/ha-active-standby.md).
const (
	DefaultLockTTL           = 15 * time.Second
	DefaultLockRenewInterval = 5 * time.Second
	DefaultLockRetryInterval = 500 * time.Millisecond
)

func (o LockOptions) withDefaults() LockOptions {
	if o.TTL <= 0 {
		o.TTL = DefaultLockTTL
	}
	if o.RenewInterval <= 0 {
		o.RenewInterval = DefaultLockRenewInterval
	}
	if o.RetryInterval <= 0 {
		o.RetryInterval = DefaultLockRetryInterval
	}
	return o
}
