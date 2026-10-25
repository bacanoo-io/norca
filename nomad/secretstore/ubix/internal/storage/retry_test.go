package storage

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// tempNetErr is a transient network error (satisfies net.Error), as a dial
// failure to a briefly-unreachable database would produce.
type tempNetErr struct{}

func (tempNetErr) Error() string   { return "dial tcp 10.0.0.1:3306: connection refused" }
func (tempNetErr) Timeout() bool   { return false }
func (tempNetErr) Temporary() bool { return true }

// flakyBackend fails the next failN operations with err, then delegates to inner.
// calls counts every operation attempt across Get/Put/Delete/List.
type flakyBackend struct {
	inner Backend
	failN int
	err   error
	calls int
}

func (f *flakyBackend) attempt() (bool, error) {
	f.calls++
	if f.failN > 0 {
		f.failN--
		return false, f.err
	}
	return true, nil
}

func (f *flakyBackend) Get(ctx context.Context, key string) (*Entry, error) {
	if ok, err := f.attempt(); !ok {
		return nil, err
	}
	return f.inner.Get(ctx, key)
}
func (f *flakyBackend) Put(ctx context.Context, e *Entry) error {
	if ok, err := f.attempt(); !ok {
		return err
	}
	return f.inner.Put(ctx, e)
}
func (f *flakyBackend) Delete(ctx context.Context, key string) error {
	if ok, err := f.attempt(); !ok {
		return err
	}
	return f.inner.Delete(ctx, key)
}
func (f *flakyBackend) List(ctx context.Context, prefix string) ([]string, error) {
	if ok, err := f.attempt(); !ok {
		return nil, err
	}
	return f.inner.List(ctx, prefix)
}

// fastRetry wraps b with near-zero backoff so tests don't sleep.
func fastRetry(inner Backend) *RetryBackend {
	r := NewRetryBackend(inner)
	r.baseDelay = time.Millisecond
	r.maxDelay = time.Millisecond
	return r
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryBackend()
	_ = mem.Put(ctx, &Entry{Key: "app/db", Value: []byte("cipher")})

	fb := &flakyBackend{inner: mem, failN: 2, err: tempNetErr{}} // fail twice, then serve
	r := fastRetry(fb)

	got, err := r.Get(ctx, "app/db")
	if err != nil {
		t.Fatalf("Get after 2 transient failures = %v, want success", err)
	}
	if got == nil || string(got.Value) != "cipher" {
		t.Fatalf("Get value = %v, want cipher", got)
	}
	if fb.calls != 3 {
		t.Fatalf("attempts = %d, want 3 (2 fail + 1 success)", fb.calls)
	}
}

func TestRetryExhaustionReturnsUnavailable(t *testing.T) {
	ctx := context.Background()
	fb := &flakyBackend{inner: NewMemoryBackend(), failN: 100, err: tempNetErr{}} // never recovers
	r := fastRetry(fb)

	_, err := r.Get(ctx, "k")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("exhausted retry = %v, want ErrUnavailable", err)
	}
	if fb.calls != r.maxAttempts {
		t.Fatalf("attempts = %d, want maxAttempts %d", fb.calls, r.maxAttempts)
	}
}

func TestPermanentErrorNotRetried(t *testing.T) {
	ctx := context.Background()
	fb := &flakyBackend{inner: NewMemoryBackend(), failN: 100, err: ErrInvalidKey} // permanent
	r := fastRetry(fb)

	_, err := r.Get(ctx, "k")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("permanent error = %v, want ErrInvalidKey (unchanged)", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatal("permanent error must not be wrapped as ErrUnavailable")
	}
	if fb.calls != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on a permanent error)", fb.calls)
	}
}

// TestFencedWriteNotRetried: a fenced write means this replica lost the HA lock.
// Retrying cannot fix that, and must not turn it into ErrUnavailable (a 503 that
// reads as "storage is down") — the replica has to see ErrFenced and step down.
func TestFencedWriteNotRetried(t *testing.T) {
	ctx := context.Background()
	fb := &flakyBackend{inner: NewMemoryBackend(), failN: 100,
		err: fmt.Errorf("storage: mysql put: %w", ErrFenced)} // as MySQLBackend wraps it
	r := fastRetry(fb)

	err := r.Put(ctx, &Entry{Key: "k", Value: []byte("v")})
	if !errors.Is(err, ErrFenced) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("fenced Put = %v, want ErrFenced unwrapped by retry", err)
	}
	if fb.calls != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on a fenced write)", fb.calls)
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	fb := &flakyBackend{inner: NewMemoryBackend(), failN: 100, err: tempNetErr{}}
	r := fastRetry(fb)

	_, err := r.Get(ctx, "k")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("canceled retry = %v, want ErrUnavailable", err)
	}
	// With the context already canceled, it must not burn all attempts.
	if fb.calls > 1 {
		t.Fatalf("attempts = %d, want 1 (stop once context is done)", fb.calls)
	}
}

func TestRetryCoversAllOps(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryBackend()
	// Each op fails once (transient) then succeeds.
	for _, tc := range []struct {
		name string
		run  func(*RetryBackend) error
	}{
		{"Put", func(r *RetryBackend) error { return r.Put(ctx, &Entry{Key: "a", Value: []byte("v")}) }},
		{"Delete", func(r *RetryBackend) error { return r.Delete(ctx, "a") }},
		{"List", func(r *RetryBackend) error { _, err := r.List(ctx, ""); return err }},
	} {
		fb := &flakyBackend{inner: mem, failN: 1, err: tempNetErr{}}
		r := fastRetry(fb)
		if err := tc.run(r); err != nil {
			t.Fatalf("%s after 1 transient failure = %v, want success", tc.name, err)
		}
		if fb.calls != 2 {
			t.Fatalf("%s attempts = %d, want 2", tc.name, fb.calls)
		}
	}
}

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"net error", tempNetErr{}, true},
		{"wrapped net error", errors.New("x: " + tempNetErr{}.Error()), false}, // string only, not a net.Error
		{"driver bad conn", driver.ErrBadConn, true},
		{"mysql server gone", &mysql.MySQLError{Number: 2006}, true},
		{"mysql lost conn", &mysql.MySQLError{Number: 2013}, true},
		{"mysql duplicate key (permanent)", &mysql.MySQLError{Number: 1062}, false},
		{"invalid key (permanent)", ErrInvalidKey, false},
		{"bare context canceled", context.Canceled, false},
		{"generic error", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%s) = %v, want %v", c.name, got, c.want)
		}
	}
	// A canceled in-flight dial arrives as a *net.OpError, which is a net.Error.
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled}
	if !retryable(opErr) {
		t.Errorf("retryable(dial OpError) = false, want true")
	}
}
