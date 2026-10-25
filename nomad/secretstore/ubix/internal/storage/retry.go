package storage

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/go-sql-driver/mysql"
)

// ErrUnavailable reports that a backend operation failed because the backend was
// (transiently) unreachable and retries were exhausted — as opposed to a
// permanent error like a malformed key. Callers can branch on it with
// [errors.Is] to degrade gracefully (e.g. serve 503 rather than 500) instead of
// treating a brief outage as a hard failure. See [RetryBackend].
var ErrUnavailable = errors.New("storage: backend temporarily unavailable")

// RetryBackend wraps a [Backend] and retries operations that fail with a
// transient error (a network failure, a dropped SQL connection) using bounded
// exponential backoff, so a brief storage outage is absorbed rather than
// surfaced to every request. It is a no-op for backends that do not produce
// transient errors (file, memory): only errors classified as retryable are
// retried, and every other error is returned immediately, unchanged.
//
// The budget is deliberately small (a few hundred milliseconds): long enough to
// ride out a sub-second blip on the request-serving path, short enough that a
// caller holding a lock across a storage read (e.g. the seal-status/health path)
// is not stalled. A sustained outage therefore still fails — wrapped as
// [ErrUnavailable] — quickly, and the caller degrades (503, readiness drops)
// rather than the process dying. Retries always honor the context: a canceled or
// expired context stops them at once.
type RetryBackend struct {
	inner       Backend
	maxAttempts int           // total attempts, including the first
	baseDelay   time.Duration // backoff for the first retry; doubles thereafter
	maxDelay    time.Duration // cap on a single backoff wait
	sleep       func(ctx context.Context, d time.Duration) error
}

// NewRetryBackend wraps inner with the default retry policy (4 attempts,
// 25ms→250ms backoff).
func NewRetryBackend(inner Backend) *RetryBackend {
	b := &RetryBackend{
		inner:       inner,
		maxAttempts: 4,
		baseDelay:   25 * time.Millisecond,
		maxDelay:    250 * time.Millisecond,
	}
	b.sleep = ctxSleep
	return b
}

// Get retries a transient failure; see [RetryBackend].
func (b *RetryBackend) Get(ctx context.Context, key string) (*Entry, error) {
	var e *Entry
	err := b.do(ctx, func() error {
		var err error
		e, err = b.inner.Get(ctx, key)
		return err
	})
	return e, err
}

// Put retries a transient failure; see [RetryBackend].
func (b *RetryBackend) Put(ctx context.Context, entry *Entry) error {
	return b.do(ctx, func() error { return b.inner.Put(ctx, entry) })
}

// Delete retries a transient failure; see [RetryBackend].
func (b *RetryBackend) Delete(ctx context.Context, key string) error {
	return b.do(ctx, func() error { return b.inner.Delete(ctx, key) })
}

// List retries a transient failure; see [RetryBackend].
func (b *RetryBackend) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	err := b.do(ctx, func() error {
		var err error
		out, err = b.inner.List(ctx, prefix)
		return err
	})
	return out, err
}

// do runs fn, retrying while it returns a retryable error and attempts and the
// context remain. A permanent error is returned unchanged; a transient error
// that never clears is returned wrapped as [ErrUnavailable].
func (b *RetryBackend) do(ctx context.Context, fn func() error) error {
	var last error
	for attempt := 1; attempt <= b.maxAttempts; attempt++ {
		if attempt > 1 {
			if err := b.sleep(ctx, b.backoff(attempt)); err != nil {
				return unavailable(last) // context ended during backoff
			}
		}
		err := fn()
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return err // permanent — surface immediately, unchanged
		}
		last = err
		if ctx.Err() != nil {
			break // context is done; further attempts cannot make progress
		}
	}
	return unavailable(last)
}

// backoff is the wait before the given attempt (attempt >= 2): baseDelay doubled
// per prior retry, capped at maxDelay.
func (b *RetryBackend) backoff(attempt int) time.Duration {
	d := b.baseDelay << (attempt - 2)
	if d <= 0 || d > b.maxDelay {
		return b.maxDelay
	}
	return d
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// unavailable wraps a transient error as [ErrUnavailable], preserving its text.
func unavailable(err error) error {
	if err == nil {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// retryable reports whether err is a transient backend failure worth retrying: a
// network error (dial/read/write, including a canceled in-flight dial), a dropped
// SQL connection, or a MySQL "server gone / shutting down / out of connections"
// condition. Everything else — a malformed key, a genuine query error, or a bare
// context cancellation by the caller — is permanent and returned at once.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1040, // ER_CON_COUNT_ERROR — too many connections
			1053, // ER_SERVER_SHUTDOWN — shutdown in progress
			1203, // ER_TOO_MANY_USER_CONNECTIONS
			2006, // CR_SERVER_GONE_ERROR — server has gone away
			2013: // CR_SERVER_LOST — lost connection during query
			return true
		}
	}
	return false
}
