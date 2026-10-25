package barrier

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// TestBarrierRoundTripProperty is a randomized property test: over many random
// keys and values (including empty and binary values), Put-then-Get must return
// the value unchanged, every value must be encrypted at rest (the physical blob
// never equals the plaintext), and all previously written keys must still read
// back their latest value. Uses a fixed seed so failures are reproducible.
func TestBarrierRoundTripProperty(t *testing.T) {
	ctx := context.Background()
	b, mem, _ := newInitializedBarrier(t)
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: test-only, seeded for reproducibility

	written := make(map[string][]byte)
	for i := 0; i < 500; i++ {
		key := randKey(rng)
		val := randBytes(rng, rng.Intn(64)) // 0..63 bytes, includes empty

		if err := b.Put(ctx, &storage.Entry{Key: key, Value: val}); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
		got, err := b.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		if got == nil || !bytes.Equal(got.Value, val) {
			t.Fatalf("round-trip mismatch at %q: got %+v, want %x", key, got, val)
		}

		// Encrypted at rest: the physical value must not be the plaintext.
		raw, err := mem.Get(ctx, key)
		if err != nil {
			t.Fatalf("physical Get(%q): %v", key, err)
		}
		if raw == nil || bytes.Equal(raw.Value, val) {
			t.Fatalf("value at %q not encrypted at rest", key)
		}
		written[key] = val
	}

	// Every key still reads back its last-written value (no collisions/corruption).
	for key, want := range written {
		got, err := b.Get(ctx, key)
		if err != nil || got == nil || !bytes.Equal(got.Value, want) {
			t.Fatalf("re-read %q = %+v, %v; want %x", key, got, err, want)
		}
	}
}

// randKey builds a valid, non-reserved storage key: "kv/" plus 1-3 lowercase
// segments, so it always passes ValidateKey and never hits the barrier's core/
// namespace.
func randKey(rng *rand.Rand) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	key := "kv"
	for n := rng.Intn(3) + 1; n > 0; n-- {
		seg := make([]byte, rng.Intn(6)+1)
		for i := range seg {
			seg[i] = letters[rng.Intn(len(letters))]
		}
		key += "/" + string(seg)
	}
	return key
}

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	_, _ = rng.Read(b)
	return b
}
