package core

import (
	"context"
	"errors"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newCore() *Core {
	return New(storage.NewMemoryBackend())
}

// initCore initializes a core with the given shares/threshold and returns it
// plus the unseal keys.
func initCore(t *testing.T, shares, threshold int) (*Core, [][]byte) {
	t.Helper()
	c := newCore()
	res, err := c.Initialize(context.Background(), InitConfig{SecretShares: shares, SecretThreshold: threshold})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return c, res.Keys
}

func TestInitializeReturnsSharesAndStaysSealed(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 5, 3)

	if len(keys) != 5 {
		t.Fatalf("got %d keys, want 5", len(keys))
	}
	for i, k := range keys {
		if len(k) != masterKeySize+1 {
			t.Errorf("key %d length = %d, want %d", i, len(k), masterKeySize+1)
		}
	}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Initialized || !st.Sealed || st.Shares != 5 || st.Threshold != 3 {
		t.Fatalf("status after init = %+v", st)
	}
}

func TestUnsealProgressive(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 5, 3)

	st, _ := c.Unseal(ctx, keys[0])
	if !st.Sealed || st.Progress != 1 {
		t.Fatalf("after 1 share: %+v", st)
	}
	st, _ = c.Unseal(ctx, keys[1])
	if !st.Sealed || st.Progress != 2 {
		t.Fatalf("after 2 shares: %+v", st)
	}
	st, err := c.Unseal(ctx, keys[2])
	if err != nil {
		t.Fatalf("Unseal 3rd: %v", err)
	}
	if st.Sealed || st.Progress != 0 {
		t.Fatalf("after threshold: %+v", st)
	}
}

func TestUnsealDuplicateShareIgnored(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 5, 3)

	_, _ = c.Unseal(ctx, keys[0])
	st, _ := c.Unseal(ctx, keys[0]) // same share again
	if st.Progress != 1 {
		t.Fatalf("duplicate share advanced progress: %+v", st)
	}
}

func TestUnsealWithAnyThresholdSubset(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 5, 3)

	// Use shares 1, 3, 4 (a non-trivial subset).
	for _, i := range []int{1, 3, 4} {
		if _, err := c.Unseal(ctx, keys[i]); err != nil {
			t.Fatalf("Unseal share %d: %v", i, err)
		}
	}
	if c.Barrier().Sealed() {
		t.Fatal("barrier still sealed after threshold subset")
	}
}

func TestUnsealWrongSharesFail(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 5, 3)

	// Corrupt one share's y-values so reconstruction yields the wrong key.
	bad := append([]byte(nil), keys[2]...)
	bad[0] ^= 0xFF

	_, _ = c.Unseal(ctx, keys[0])
	_, _ = c.Unseal(ctx, keys[1])
	st, err := c.Unseal(ctx, bad)
	if !errors.Is(err, ErrUnsealFailed) {
		t.Fatalf("want ErrUnsealFailed, got %v (status %+v)", err, st)
	}
	// Progress must reset and the vault stays sealed.
	if !c.Barrier().Sealed() {
		t.Fatal("barrier unsealed with wrong shares")
	}
	status, _ := c.Status(ctx)
	if status.Progress != 0 {
		t.Fatalf("progress not reset after failure: %+v", status)
	}
}

func TestDataRoundTripThroughBarrierAfterUnseal(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 3, 2)
	for _, i := range []int{0, 1} {
		if _, err := c.Unseal(ctx, keys[i]); err != nil {
			t.Fatalf("Unseal: %v", err)
		}
	}

	b := c.Barrier()
	if err := b.Put(ctx, &storage.Entry{Key: "secret/x", Value: []byte("value")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := b.Get(ctx, "secret/x")
	if err != nil || got == nil || string(got.Value) != "value" {
		t.Fatalf("round-trip: got %+v, err %v", got, err)
	}
}

// TestPersistAcrossRestart simulates a process restart: a fresh Core over the
// same backend can be unsealed with the original shares and read prior data.
func TestPersistAcrossRestart(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()

	c1 := New(mem)
	res, err := c1.Initialize(ctx, InitConfig{SecretShares: 3, SecretThreshold: 2})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	_, _ = c1.Unseal(ctx, res.Keys[0])
	_, _ = c1.Unseal(ctx, res.Keys[1])
	if err := c1.Barrier().Put(ctx, &storage.Entry{Key: "secret/x", Value: []byte("durable")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// "Restart": new Core, same storage.
	c2 := New(mem)
	if st, _ := c2.Status(ctx); !st.Initialized || !st.Sealed {
		t.Fatalf("restarted core status = %+v, want initialized+sealed", st)
	}
	_, _ = c2.Unseal(ctx, res.Keys[0])
	if _, err := c2.Unseal(ctx, res.Keys[2]); err != nil { // different subset
		t.Fatalf("re-Unseal: %v", err)
	}
	got, err := c2.Barrier().Get(ctx, "secret/x")
	if err != nil || got == nil || string(got.Value) != "durable" {
		t.Fatalf("data after restart: got %+v, err %v", got, err)
	}
}

func TestSealAndReunseal(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 3, 2)
	_, _ = c.Unseal(ctx, keys[0])
	_, _ = c.Unseal(ctx, keys[1])

	c.Seal()
	if !c.Barrier().Sealed() {
		t.Fatal("not sealed after Seal()")
	}
	st, _ := c.Status(ctx)
	if st.Progress != 0 {
		t.Fatalf("progress not cleared after Seal: %+v", st)
	}

	_, _ = c.Unseal(ctx, keys[1])
	if _, err := c.Unseal(ctx, keys[2]); err != nil {
		t.Fatalf("re-Unseal after Seal: %v", err)
	}
	if c.Barrier().Sealed() {
		t.Fatal("still sealed after re-unseal")
	}
}

func TestInitializedReflectsState(t *testing.T) {
	ctx := context.Background()
	c := newCore()
	if ok, err := c.Initialized(ctx); err != nil || ok {
		t.Fatalf("before init: got %v, err %v", ok, err)
	}
	if _, err := c.Initialize(ctx, InitConfig{SecretShares: 3, SecretThreshold: 2}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if ok, err := c.Initialized(ctx); err != nil || !ok {
		t.Fatalf("after init: got %v, err %v", ok, err)
	}
}

func TestStatusBeforeInitialize(t *testing.T) {
	st, err := newCore().Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Initialized || !st.Sealed {
		t.Fatalf("uninitialized status = %+v", st)
	}
}

func TestInitializeTwice(t *testing.T) {
	ctx := context.Background()
	c, _ := initCore(t, 3, 2)
	if _, err := c.Initialize(ctx, InitConfig{SecretShares: 3, SecretThreshold: 2}); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Initialize: want ErrAlreadyInitialized, got %v", err)
	}
}

func TestUnsealBeforeInitialize(t *testing.T) {
	share := make([]byte, masterKeySize+1)
	if _, err := newCore().Unseal(context.Background(), share); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Unseal before init: want ErrNotInitialized, got %v", err)
	}
}

func TestInvalidInitConfig(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []InitConfig{
		{SecretShares: 1, SecretThreshold: 1},
		{SecretShares: 3, SecretThreshold: 1},
		{SecretShares: 3, SecretThreshold: 4},
		{SecretShares: 256, SecretThreshold: 2},
	} {
		if _, err := newCore().Initialize(ctx, cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("Initialize(%+v): want ErrInvalidConfig, got %v", cfg, err)
		}
	}
}

func TestInvalidShareLength(t *testing.T) {
	ctx := context.Background()
	c, _ := initCore(t, 3, 2)
	if _, err := c.Unseal(ctx, []byte("too short")); !errors.Is(err, ErrInvalidShare) {
		t.Fatalf("short share: want ErrInvalidShare, got %v", err)
	}
}

// Guard against the barrier error type drifting away from what Unseal checks.
var _ = barrier.ErrInvalidKey
