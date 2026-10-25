package transit

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newEngine() *Engine {
	return New(storage.NewMemoryBackend(), "transit")
}

func TestCreateEncryptDecrypt(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "orders"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	plaintext := []byte("card-number-4242")
	ct, err := e.Encrypt(ctx, "orders", plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(ct, "ubix:v1:") {
		t.Fatalf("ciphertext = %q, want ubix:v1: prefix", ct)
	}
	got, err := e.Decrypt(ctx, "orders", ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip = %q, want %q", got, plaintext)
	}
}

func TestKeyMaterialNeverReturned(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	info, _ := e.CreateKey(ctx, "k")
	// KeyInfo exposes only metadata; there is no field for material.
	if info.Name != "k" || info.LatestVersion != 1 || len(info.Versions) != 1 {
		t.Fatalf("KeyInfo = %+v", info)
	}
}

func TestRotationKeepsOldCiphertextReadable(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKey(ctx, "k")

	ctV1, _ := e.Encrypt(ctx, "k", []byte("v1-data"))

	if _, err := e.Rotate(ctx, "k"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	// New encryptions use v2.
	ctV2, _ := e.Encrypt(ctx, "k", []byte("v2-data"))
	if !strings.HasPrefix(ctV2, "ubix:v2:") {
		t.Fatalf("post-rotate ciphertext = %q, want v2", ctV2)
	}
	// Old v1 ciphertext still decrypts.
	got, err := e.Decrypt(ctx, "k", ctV1)
	if err != nil || string(got) != "v1-data" {
		t.Fatalf("decrypt v1 after rotate: got %q err %v", got, err)
	}
}

func TestCiphertextBoundToKeyName(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKey(ctx, "a")
	_, _ = e.CreateKey(ctx, "b")

	ct, _ := e.Encrypt(ctx, "a", []byte("secret"))
	// Decrypting under a different key must fail (name is authenticated data).
	if _, err := e.Decrypt(ctx, "b", ct); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("cross-key decrypt: want ErrInvalidCiphertext, got %v", err)
	}
}

func TestTamperedCiphertextRejected(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKey(ctx, "k")
	ct, _ := e.Encrypt(ctx, "k", []byte("data"))

	// Corrupt the base64 payload.
	tampered := ct[:len(ct)-4] + "AAAA"
	if _, err := e.Decrypt(ctx, "k", tampered); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
}

func TestCreateDuplicate(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKey(ctx, "k")
	if _, err := e.CreateKey(ctx, "k"); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("duplicate create: want ErrKeyExists, got %v", err)
	}
}

func TestEncryptUnknownKey(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.Encrypt(ctx, "nope", []byte("x")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("want ErrKeyNotFound, got %v", err)
	}
}

func TestInvalidCiphertextFormats(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKey(ctx, "k")
	for _, bad := range []string{"", "plain", "ubix:", "ubix:v:x", "ubix:vX:x", "ubix:v1:not-base64!"} {
		if _, err := e.Decrypt(ctx, "k", bad); !errors.Is(err, ErrInvalidCiphertext) {
			t.Errorf("Decrypt(%q): want ErrInvalidCiphertext, got %v", bad, err)
		}
	}
}

func TestListAndDeleteKeys(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKey(ctx, "a")
	_, _ = e.CreateKey(ctx, "b")

	names, err := e.ListKeys(ctx)
	if err != nil || len(names) != 2 {
		t.Fatalf("ListKeys = %v, err %v", names, err)
	}
	if err := e.DeleteKey(ctx, "a"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	if _, err := e.ReadKey(ctx, "a"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("read deleted key: want ErrKeyNotFound, got %v", err)
	}
}

func TestInvalidNames(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	for _, name := range []string{"", "has/slash"} {
		if _, err := e.CreateKey(ctx, name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("CreateKey(%q): want ErrInvalidName, got %v", name, err)
		}
	}
}

// TestKeyMaterialEncryptedAtRest mounts the engine on a barrier and confirms raw
// key material never appears in the underlying store.
func TestKeyMaterialEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	b := barrier.New(mem)
	mk := make([]byte, 32)
	if _, err := rand.Read(mk); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := b.Initialize(ctx, mk); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := b.Unseal(ctx, mk); err != nil {
		t.Fatalf("Unseal: %v", err)
	}

	e := New(b, "transit")
	info, err := e.CreateKey(ctx, "k")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	_ = info

	// Encrypt/decrypt round-trips through the barrier-backed engine.
	ct, _ := e.Encrypt(ctx, "k", []byte("hello"))
	got, err := e.Decrypt(ctx, "k", ct)
	if err != nil || string(got) != "hello" {
		t.Fatalf("round-trip through barrier: got %q err %v", got, err)
	}

	// The raw stored key record must be ciphertext (barrier-encrypted).
	raw, _ := mem.Get(ctx, "transit/key/k")
	if raw == nil {
		t.Fatal("expected a stored key record")
	}
	// A freshly-generated random key won't match a fixed string, so assert the
	// stored blob is not valid JSON (i.e. it is encrypted, not the plain record).
	if bytes.Contains(raw.Value, []byte(`"versions"`)) {
		t.Fatal("key record stored in plaintext JSON — not encrypted at rest")
	}
}
