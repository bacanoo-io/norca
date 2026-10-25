package wrapping

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newStore() *Store { return NewStore(storage.NewMemoryBackend()) }

func TestWrapUnwrapRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	payload := json.RawMessage(`{"password":"s3cr3t","user":"alice"}`)

	info, err := s.Wrap(ctx, payload, time.Minute)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if !strings.HasPrefix(info.Token, "uvw.") {
		t.Fatalf("token = %q, want uvw. prefix", info.Token)
	}
	if info.TTL != time.Minute {
		t.Fatalf("ttl = %v, want 1m", info.TTL)
	}

	got, err := s.Unwrap(ctx, info.Token)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("unwrapped = %s, want %s", got, payload)
	}
}

func TestUnwrapIsSingleUse(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	info, err := s.Wrap(ctx, json.RawMessage(`{"k":"v"}`), time.Minute)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if _, err := s.Unwrap(ctx, info.Token); err != nil {
		t.Fatalf("first Unwrap: %v", err)
	}
	// Second unwrap of the same token must fail — it was destroyed.
	if _, err := s.Unwrap(ctx, info.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Unwrap = %v, want ErrNotFound", err)
	}
}

// slowGetStorage widens the window between an unwrap's read and its delete, the
// window a concurrent unwrap of the same token would race through.
type slowGetStorage struct {
	Storage
}

func (s slowGetStorage) Get(ctx context.Context, key string) (*storage.Entry, error) {
	e, err := s.Storage.Get(ctx, key)
	time.Sleep(10 * time.Millisecond)
	return e, err
}

func TestUnwrapIsSingleUseUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := NewStore(slowGetStorage{storage.NewMemoryBackend()})
	info, err := s.Wrap(ctx, json.RawMessage(`{"k":"v"}`), time.Minute)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Unwrap(ctx, info.Token)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	succeeded := 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case !errors.Is(err, ErrNotFound):
			t.Fatalf("Unwrap = %v, want nil or ErrNotFound", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent unwraps returned the payload, want exactly 1", succeeded)
	}
}

func TestUnwrapUnknownToken(t *testing.T) {
	if _, err := newStore().Unwrap(context.Background(), "uvw.deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Unwrap unknown = %v, want ErrNotFound", err)
	}
}

func TestUnwrapExpired(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	// Freeze time so we can advance past the TTL deterministically.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	info, err := s.Wrap(ctx, json.RawMessage(`{"k":"v"}`), 30*time.Second)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	s.now = func() time.Time { return base.Add(time.Minute) } // past expiry
	if _, err := s.Unwrap(ctx, info.Token); !errors.Is(err, ErrExpired) {
		t.Fatalf("Unwrap expired = %v, want ErrExpired", err)
	}
	// And the expired record is gone.
	if _, err := s.Unwrap(ctx, info.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Unwrap after expiry cleanup = %v, want ErrNotFound", err)
	}
}

func TestWrapDefaultTTL(t *testing.T) {
	info, err := newStore().Wrap(context.Background(), json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if info.TTL != DefaultTTL {
		t.Fatalf("default ttl = %v, want %v", info.TTL, DefaultTTL)
	}
}

func TestWrapTTLTooLong(t *testing.T) {
	if _, err := newStore().Wrap(context.Background(), json.RawMessage(`{}`), MaxTTL+time.Hour); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("Wrap over-max = %v, want ErrInvalidTTL", err)
	}
}

func TestWrappedTokensAreDistinct(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	a, _ := s.Wrap(ctx, json.RawMessage(`{}`), time.Minute)
	b, _ := s.Wrap(ctx, json.RawMessage(`{}`), time.Minute)
	if a.Token == b.Token {
		t.Fatal("two wrap tokens were identical")
	}
}
