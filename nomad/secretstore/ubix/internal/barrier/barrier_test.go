package barrier

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// newKey returns a random 32-byte key for tests.
func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

// newInitializedBarrier returns an unsealed barrier over a fresh in-memory
// backend, plus the backend (for inspecting ciphertext at rest) and master key.
func newInitializedBarrier(t *testing.T) (*Barrier, *storage.MemoryBackend, []byte) {
	t.Helper()
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	b := New(mem)
	master := newKey(t)
	if err := b.Initialize(ctx, master); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := b.Unseal(ctx, master); err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	return b, mem, master
}

func TestInitializeLeavesSealedThenUnseals(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	b := New(mem)
	master := newKey(t)

	if err := b.Initialize(ctx, master); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !b.Sealed() {
		t.Fatal("barrier should be sealed immediately after Initialize")
	}
	if err := b.Unseal(ctx, master); err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if b.Sealed() {
		t.Fatal("barrier should be unsealed after Unseal")
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newInitializedBarrier(t)

	want := []byte("a very secret value")
	if err := b.Put(ctx, &storage.Entry{Key: "secret/data/x", Value: want}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := b.Get(ctx, "secret/data/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || !bytes.Equal(got.Value, want) {
		t.Fatalf("round-trip: got %+v, want %q", got, want)
	}
}

func TestGetMissingReturnsNil(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newInitializedBarrier(t)
	got, err := b.Get(ctx, "secret/nope")
	if err != nil || got != nil {
		t.Fatalf("Get missing: got %+v, err %v", got, err)
	}
}

// TestEncryptedAtRest confirms the underlying backend never sees plaintext and
// that the stored blob carries the format version byte.
func TestEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	b, mem, _ := newInitializedBarrier(t)

	secret := []byte("plaintext-should-never-appear")
	if err := b.Put(ctx, &storage.Entry{Key: "secret/x", Value: secret}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	raw, err := mem.Get(ctx, "secret/x")
	if err != nil || raw == nil {
		t.Fatalf("raw Get: %+v, %v", raw, err)
	}
	if bytes.Contains(raw.Value, secret) {
		t.Fatal("plaintext found in stored ciphertext")
	}
	if raw.Value[0] != formatVersion {
		t.Fatalf("stored blob version = %d, want %d", raw.Value[0], formatVersion)
	}
}

// TestTamperDetection flips a byte of the stored ciphertext; Get must fail the
// GCM authentication check rather than return corrupt data.
func TestTamperDetection(t *testing.T) {
	ctx := context.Background()
	b, mem, _ := newInitializedBarrier(t)

	if err := b.Put(ctx, &storage.Entry{Key: "secret/x", Value: []byte("data")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	raw, _ := mem.Get(ctx, "secret/x")
	raw.Value[len(raw.Value)-1] ^= 0xFF // corrupt the auth tag
	if err := mem.Put(ctx, raw); err != nil {
		t.Fatalf("re-Put tampered: %v", err)
	}

	if _, err := b.Get(ctx, "secret/x"); err == nil {
		t.Fatal("Get returned no error on tampered ciphertext")
	}
}

// TestNonceUniqueness ensures encrypting identical plaintext twice yields
// different stored blobs (fresh random nonce each time).
func TestNonceUniqueness(t *testing.T) {
	ctx := context.Background()
	b, mem, _ := newInitializedBarrier(t)

	same := []byte("identical")
	_ = b.Put(ctx, &storage.Entry{Key: "a", Value: same})
	first, _ := mem.Get(ctx, "a")
	blobA := append([]byte(nil), first.Value...)

	_ = b.Put(ctx, &storage.Entry{Key: "a", Value: same})
	second, _ := mem.Get(ctx, "a")

	if bytes.Equal(blobA, second.Value) {
		t.Fatal("identical plaintext produced identical ciphertext (nonce reuse)")
	}
}

// TestCiphertextCannotBeRelocated verifies the path is bound into the AEAD: a
// valid blob copied from one path to another must fail to decrypt at the new
// path, defeating a storage-level relocation attack.
func TestCiphertextCannotBeRelocated(t *testing.T) {
	ctx := context.Background()
	b, mem, _ := newInitializedBarrier(t)

	if err := b.Put(ctx, &storage.Entry{Key: "secret/source", Value: []byte("moved")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Copy the raw ciphertext to a different path, bypassing the barrier.
	raw, _ := mem.Get(ctx, "secret/source")
	if err := mem.Put(ctx, &storage.Entry{Key: "secret/dest", Value: raw.Value}); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	// Sanity: it still decrypts at its original path.
	if _, err := b.Get(ctx, "secret/source"); err != nil {
		t.Fatalf("Get at original path: %v", err)
	}
	// But not at the path it was relocated to.
	if _, err := b.Get(ctx, "secret/dest"); err == nil {
		t.Fatal("relocated ciphertext decrypted at the wrong path")
	}
}

func TestOperationsFailWhileSealed(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	b := New(mem)
	if err := b.Initialize(ctx, newKey(t)); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// Not unsealed.
	if _, err := b.Get(ctx, "k"); !errors.Is(err, ErrSealed) {
		t.Errorf("Get sealed: want ErrSealed, got %v", err)
	}
	if err := b.Put(ctx, &storage.Entry{Key: "k", Value: []byte("v")}); !errors.Is(err, ErrSealed) {
		t.Errorf("Put sealed: want ErrSealed, got %v", err)
	}
	if err := b.Delete(ctx, "k"); !errors.Is(err, ErrSealed) {
		t.Errorf("Delete sealed: want ErrSealed, got %v", err)
	}
	if _, err := b.List(ctx, ""); !errors.Is(err, ErrSealed) {
		t.Errorf("List sealed: want ErrSealed, got %v", err)
	}
}

func TestSealAfterUnseal(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newInitializedBarrier(t)
	_ = b.Put(ctx, &storage.Entry{Key: "k", Value: []byte("v")})

	b.Seal()
	if !b.Sealed() {
		t.Fatal("Sealed() false after Seal()")
	}
	if _, err := b.Get(ctx, "k"); !errors.Is(err, ErrSealed) {
		t.Errorf("Get after Seal: want ErrSealed, got %v", err)
	}
}

func TestUnsealWrongKey(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	b := New(mem)
	if err := b.Initialize(ctx, newKey(t)); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := b.Unseal(ctx, newKey(t)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Unseal wrong key: want ErrInvalidKey, got %v", err)
	}
	if !b.Sealed() {
		t.Fatal("barrier must remain sealed after a failed unseal")
	}
}

func TestUnsealBeforeInitialize(t *testing.T) {
	ctx := context.Background()
	b := New(storage.NewMemoryBackend())
	if err := b.Unseal(ctx, newKey(t)); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Unseal uninitialized: want ErrNotInitialized, got %v", err)
	}
}

func TestInitializeTwice(t *testing.T) {
	ctx := context.Background()
	b := New(storage.NewMemoryBackend())
	master := newKey(t)
	if err := b.Initialize(ctx, master); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := b.Initialize(ctx, master); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Initialize: want ErrAlreadyInitialized, got %v", err)
	}
}

// TestPersistsAcrossReseal proves the barrier key survives sealing: a new
// Barrier over the same backend can Unseal with the same master key and read
// data written earlier.
func TestPersistsAcrossReseal(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	master := newKey(t)

	b1 := New(mem)
	if err := b1.Initialize(ctx, master); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := b1.Unseal(ctx, master); err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if err := b1.Put(ctx, &storage.Entry{Key: "secret/x", Value: []byte("durable")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	b2 := New(mem)
	if err := b2.Unseal(ctx, master); err != nil {
		t.Fatalf("re-Unseal: %v", err)
	}
	got, err := b2.Get(ctx, "secret/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || string(got.Value) != "durable" {
		t.Fatalf("persisted value = %+v, want durable", got)
	}
}

func TestReservedPathRejected(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newInitializedBarrier(t)

	for _, key := range []string{keyringPath, "core/anything"} {
		if _, err := b.Get(ctx, key); !errors.Is(err, ErrReservedPath) {
			t.Errorf("Get(%q): want ErrReservedPath, got %v", key, err)
		}
		if err := b.Put(ctx, &storage.Entry{Key: key, Value: []byte("x")}); !errors.Is(err, ErrReservedPath) {
			t.Errorf("Put(%q): want ErrReservedPath, got %v", key, err)
		}
		if err := b.Delete(ctx, key); !errors.Is(err, ErrReservedPath) {
			t.Errorf("Delete(%q): want ErrReservedPath, got %v", key, err)
		}
	}
}

// TestListHidesReservedNamespace verifies the barrier's internal "core/" entry
// does not leak through List at the root.
func TestListHidesReservedNamespace(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newInitializedBarrier(t)
	_ = b.Put(ctx, &storage.Entry{Key: "secret/x", Value: []byte("v")})

	children, err := b.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, c := range children {
		if c == reservedPrefix {
			t.Fatalf("List(\"\") leaked reserved namespace: %v", children)
		}
	}
}

func TestInitializedReflectsState(t *testing.T) {
	ctx := context.Background()
	b := New(storage.NewMemoryBackend())

	if ok, err := b.Initialized(ctx); err != nil || ok {
		t.Fatalf("Initialized before init: got %v, err %v", ok, err)
	}
	if err := b.Initialize(ctx, newKey(t)); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if ok, err := b.Initialized(ctx); err != nil || !ok {
		t.Fatalf("Initialized after init: got %v, err %v", ok, err)
	}
}

func TestDeleteRemoves(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newInitializedBarrier(t)

	_ = b.Put(ctx, &storage.Entry{Key: "secret/x", Value: []byte("v")})
	if err := b.Delete(ctx, "secret/x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := b.Get(ctx, "secret/x")
	if err != nil || got != nil {
		t.Fatalf("after Delete: got %+v, err %v", got, err)
	}
}

func TestDataKeyValidation(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newInitializedBarrier(t)

	// Malformed keys are rejected as invalid, distinct from reserved-path errors.
	for _, k := range []string{"", "a//b", "a/../b"} {
		if _, err := b.Get(ctx, k); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Get(%q): want ErrInvalidKey, got %v", k, err)
		}
	}
}

func TestInvalidKeyLengths(t *testing.T) {
	ctx := context.Background()
	b := New(storage.NewMemoryBackend())
	for _, bad := range [][]byte{nil, make([]byte, 16), make([]byte, 31), make([]byte, 33)} {
		if err := b.Initialize(ctx, bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Initialize(len=%d): want ErrInvalidKey, got %v", len(bad), err)
		}
	}
}
