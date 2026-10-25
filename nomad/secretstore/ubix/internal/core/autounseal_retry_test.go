package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/seal"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// flakySeal fails Unwrap until failures reaches zero, then delegates — a KMS
// that is unreachable for the first few attempts.
type flakySeal struct {
	seal.Seal
	failures atomic.Int32
}

var errKMSDown = errors.New("kms unreachable")

func (s *flakySeal) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	if s.failures.Add(-1) >= 0 {
		return nil, errKMSDown
	}
	return s.Seal.Unwrap(ctx, wrapped)
}

// initAutoVault initializes an auto-unseal vault on mem and returns its KEK.
func initAutoVault(t *testing.T, mem storage.Backend) []byte {
	t.Helper()
	kek := newKEK(t)
	if _, err := New(mem, WithAutoUnsealKey(kek)).Initialize(context.Background(), InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return kek
}

func TestRunAutoUnsealRetriesUntilSealReachable(t *testing.T) {
	mem := storage.NewMemoryBackend()
	kek := initAutoVault(t, mem)

	fs := &flakySeal{Seal: seal.NewStaticKEK(kek)}
	fs.failures.Store(3)
	c := New(mem, WithSeal(fs))

	var waits []time.Duration
	err := c.RunAutoUnseal(context.Background(), time.Millisecond, 4*time.Millisecond,
		func(err error, next time.Duration) {
			if !errors.Is(err, errKMSDown) {
				t.Errorf("onFailure err = %v, want errKMSDown", err)
			}
			waits = append(waits, next)
		})
	if err != nil {
		t.Fatalf("RunAutoUnseal: %v", err)
	}
	if c.Barrier().Sealed() {
		t.Fatal("still sealed after RunAutoUnseal returned nil")
	}
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
}

func TestRunAutoUnsealBackoffIsCapped(t *testing.T) {
	mem := storage.NewMemoryBackend()
	kek := initAutoVault(t, mem)

	fs := &flakySeal{Seal: seal.NewStaticKEK(kek)}
	fs.failures.Store(5)
	c := New(mem, WithSeal(fs))

	var last time.Duration
	if err := c.RunAutoUnseal(context.Background(), time.Millisecond, 2*time.Millisecond,
		func(_ error, next time.Duration) { last = next }); err != nil {
		t.Fatalf("RunAutoUnseal: %v", err)
	}
	if last != 2*time.Millisecond {
		t.Fatalf("last wait = %v, want the 2ms cap", last)
	}
}

func TestRunAutoUnsealStopsOnShamirVault(t *testing.T) {
	mem := storage.NewMemoryBackend()
	if _, err := New(mem).Initialize(context.Background(), InitConfig{SecretShares: 3, SecretThreshold: 2}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c := New(mem, WithAutoUnsealKey(newKEK(t)))

	calls := 0
	err := c.RunAutoUnseal(context.Background(), time.Millisecond, time.Millisecond,
		func(error, time.Duration) { calls++ })
	if !errors.Is(err, ErrNotAutoUnseal) {
		t.Fatalf("RunAutoUnseal = %v, want ErrNotAutoUnseal", err)
	}
	if calls != 0 {
		t.Fatalf("retried %d times on a permanent error, want 0", calls)
	}
}

func TestRunAutoUnsealStopsOnContextCancel(t *testing.T) {
	mem := storage.NewMemoryBackend()
	kek := initAutoVault(t, mem)

	fs := &flakySeal{Seal: seal.NewStaticKEK(kek)}
	fs.failures.Store(1 << 30) // never recovers
	c := New(mem, WithSeal(fs))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.RunAutoUnseal(ctx, time.Millisecond, time.Millisecond, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunAutoUnseal = %v, want context.DeadlineExceeded", err)
	}
	if !c.Barrier().Sealed() {
		t.Fatal("unsealed with a seal that never recovers")
	}
}

func TestRunAutoUnsealWaitsForInitialization(t *testing.T) {
	mem := storage.NewMemoryBackend()
	c := New(mem, WithAutoUnsealKey(newKEK(t)))

	var uninit atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- c.RunAutoUnseal(context.Background(), time.Millisecond, time.Millisecond,
			func(err error, _ time.Duration) {
				if errors.Is(err, ErrNotInitialized) {
					uninit.Add(1)
				}
			})
	}()
	for uninit.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// An operator initializes through the API; auto-unseal init leaves it unsealed.
	if _, err := c.Initialize(context.Background(), InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunAutoUnseal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunAutoUnseal did not return after the vault was initialized")
	}
}
