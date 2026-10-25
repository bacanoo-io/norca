package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// mysqlLockTable holds one row per HA lock. Expiry is always compared against
// the database's NOW(6), never a replica's clock, so clock skew between
// replicas cannot hand the lock to two holders (docs/design/ha-active-standby.md).
const mysqlLockTable = "ubixvault_lock"

// mysqlLockEpoch is the expires_at of a lock nobody holds.
const mysqlLockEpoch = "1970-01-01 00:00:00"

// fenceToken identifies the acquisition a replica's writes are fenced to.
type fenceToken struct {
	name   string
	holder string
	gen    uint64
}

// HALock returns this replica's handle on the named lock. See [HABackend]. The
// first handle arms fencing: from then on this backend's writes succeed only
// while the lock is held, so a standby that has never been active cannot write
// either.
func (b *MySQLBackend) HALock(name, holderID, advertise string, opts LockOptions) Lock {
	b.fenceMu.Lock()
	if b.fence == nil {
		b.fence = &fenceToken{name: name, holder: holderID} // generation 0 is never held
	}
	b.fenceMu.Unlock()
	return &mysqlLock{b: b, name: name, holder: holderID, advertise: advertise, opts: opts.withDefaults()}
}

var _ HABackend = (*MySQLBackend)(nil)

// setFence fences every later write to acquisition f.
func (b *MySQLBackend) setFence(f fenceToken) {
	b.fenceMu.Lock()
	defer b.fenceMu.Unlock()
	b.fence = &f
}

func (b *MySQLBackend) currentFence() *fenceToken {
	b.fenceMu.RLock()
	defer b.fenceMu.RUnlock()
	return b.fence
}

// exec runs a write. Before this replica has ever held an HA lock it is a plain
// statement, exactly as before HA existed. After, it runs in a transaction that
// first takes a shared lock on the HA lock row and checks that this replica
// still holds the acquisition it was fenced to; otherwise it fails with
// [ErrFenced]. An acquiring replica's generation bump needs an exclusive lock on
// that row, so it waits for in-flight fenced writes to commit, and every write
// after it fails the check — the old and new active replica can never
// interleave writes. (LOCK IN SHARE MODE rather than FOR SHARE: the latter is
// MySQL 8 only, and MariaDB is the reference database.)
func (b *MySQLBackend) exec(ctx context.Context, query string, args ...any) error {
	f := b.currentFence()
	if f == nil {
		_, err := b.db.ExecContext(ctx, query, args...)
		return err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var holder []byte
	var gen uint64
	err = tx.QueryRowContext(ctx,
		"SELECT holder_id, generation FROM "+mysqlLockTable+" WHERE name = ? LOCK IN SHARE MODE",
		f.name).Scan(&holder, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	if string(holder) != f.holder || gen != f.gen {
		return ErrFenced
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// mysqlLock is one replica's handle on an HA lock row.
type mysqlLock struct {
	b         *MySQLBackend
	name      string
	holder    string
	advertise string
	opts      LockOptions

	mu   sync.Mutex
	held *heldLock // current acquisition, nil if none
}

// heldLock is one acquisition and its renewal goroutine.
type heldLock struct {
	gen      uint64
	lost     chan struct{}
	lostOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

func (h *heldLock) markLost() { h.lostOnce.Do(func() { close(h.lost) }) }

func (h *heldLock) isLost() bool {
	select {
	case <-h.lost:
		return true
	default:
		return false
	}
}

// Acquire implements [Lock]. Transient database errors are retried like a held
// lock, so a standby keeps waiting through a database blip; a permanent error
// is returned.
func (l *mysqlLock) Acquire(ctx context.Context) (<-chan struct{}, error) {
	l.mu.Lock()
	if h := l.held; h != nil {
		if !h.isLost() {
			l.mu.Unlock()
			return h.lost, nil
		}
		l.held = nil // a previous acquisition was lost; start afresh
	}
	l.mu.Unlock()

	for {
		start := time.Now()
		gen, ok, err := l.tryAcquire(ctx)
		switch {
		case err == nil && ok:
			return l.hold(gen, start), nil
		case err != nil && !retryable(err) && ctx.Err() == nil:
			return nil, err
		}
		if err := ctxSleep(ctx, l.opts.RetryInterval); err != nil {
			return nil, err
		}
	}
}

// ensureRow creates the lock row, unheld, if it does not exist yet.
func (l *mysqlLock) ensureRow(ctx context.Context) error {
	_, err := l.b.db.ExecContext(ctx,
		"INSERT IGNORE INTO "+mysqlLockTable+" (name, holder_id, advertise, generation, expires_at) "+
			"VALUES (?, '', '', 0, '"+mysqlLockEpoch+"')", l.name)
	return err
}

// tryAcquire takes the lock if nobody holds it or the holder's lease has
// expired, bumping the generation, and reports the new generation.
func (l *mysqlLock) tryAcquire(ctx context.Context) (uint64, bool, error) {
	if err := l.ensureRow(ctx); err != nil {
		return 0, false, err
	}
	tx, err := l.b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		"UPDATE "+mysqlLockTable+" SET holder_id = ?, advertise = ?, generation = generation + 1, "+
			"expires_at = NOW(6) + INTERVAL ? MICROSECOND "+
			"WHERE name = ? AND (holder_id = '' OR expires_at < NOW(6))",
		l.holder, l.advertise, l.opts.TTL.Microseconds(), l.name)
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return 0, false, err
	}
	var gen uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT generation FROM "+mysqlLockTable+" WHERE name = ?", l.name).Scan(&gen); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return gen, true, nil
}

// hold records acquisition gen (whose acquiring statement started at start),
// fences this replica's writes to it, and starts renewing it.
func (l *mysqlLock) hold(gen uint64, start time.Time) <-chan struct{} {
	h := &heldLock{
		gen:  gen,
		lost: make(chan struct{}),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	l.b.setFence(fenceToken{name: l.name, holder: l.holder, gen: gen})
	l.mu.Lock()
	l.held = h
	l.mu.Unlock()
	go l.renew(h, start)
	return h.lost
}

// renew extends the lease every RenewInterval until stopped. The lock is lost
// when a renewal finds another holder, or when TTL-RenewInterval passes (on the
// replica's monotonic clock, measured from the start of the last successful
// renewal) without one — before the lease itself can run out in the database.
func (l *mysqlLock) renew(h *heldLock, lastOK time.Time) {
	defer close(h.done)
	deadline := l.opts.TTL - l.opts.RenewInterval
	t := time.NewTicker(l.opts.RenewInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), l.opts.RenewInterval)
		res, err := l.b.db.ExecContext(ctx,
			"UPDATE "+mysqlLockTable+" SET expires_at = NOW(6) + INTERVAL ? MICROSECOND "+
				"WHERE name = ? AND holder_id = ? AND generation = ?",
			l.opts.TTL.Microseconds(), l.name, l.holder, h.gen)
		cancel()
		if err == nil {
			// expires_at always moves forward, so a matched row is always changed:
			// zero rows affected means the lock is no longer ours.
			if n, rerr := res.RowsAffected(); rerr == nil && n == 0 {
				h.markLost()
				return
			}
			lastOK = start
			continue
		}
		if time.Since(lastOK) >= deadline {
			h.markLost()
			return
		}
	}
}

// Release implements [Lock].
func (l *mysqlLock) Release(ctx context.Context) error {
	l.mu.Lock()
	h := l.held
	l.held = nil
	l.mu.Unlock()
	if h == nil {
		return nil
	}
	close(h.stop)
	<-h.done
	h.markLost()

	_, err := l.b.db.ExecContext(ctx,
		"UPDATE "+mysqlLockTable+" SET holder_id = '', advertise = '', expires_at = '"+mysqlLockEpoch+"' "+
			"WHERE name = ? AND holder_id = ? AND generation = ?",
		l.name, l.holder, h.gen)
	if err != nil {
		return fmt.Errorf("storage: release ha lock: %w", err)
	}
	return nil
}

// Holder implements [Lock].
func (l *mysqlLock) Holder(ctx context.Context) (Holder, error) {
	var id, adv []byte
	var gen uint64
	err := l.b.db.QueryRowContext(ctx,
		"SELECT holder_id, advertise, generation FROM "+mysqlLockTable+
			" WHERE name = ? AND holder_id <> '' AND expires_at >= NOW(6)", l.name).Scan(&id, &adv, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return Holder{}, nil
	}
	if err != nil {
		return Holder{}, fmt.Errorf("storage: read ha lock holder: %w", err)
	}
	return Holder{ID: string(id), Advertise: string(adv), Generation: gen}, nil
}
