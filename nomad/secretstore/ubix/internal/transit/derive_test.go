package transit

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestDerivedRoundTripAndContextBinding(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKeyWithOptions(ctx, "k", KeyTypeAES256, KeyOptions{Derived: true}); err != nil {
		t.Fatalf("create derived: %v", err)
	}

	dctx := []byte("tenant-a")
	pt := []byte("secret")
	ciph, err := e.EncryptWithContext(ctx, "k", pt, dctx)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := e.DecryptWithContext(ctx, "k", ciph, dctx)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
	// Wrong context must fail (the context is bound into the derivation and AAD).
	if _, err := e.DecryptWithContext(ctx, "k", ciph, []byte("tenant-b")); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("decrypt with wrong context = %v, want ErrInvalidCiphertext", err)
	}
}

func TestDerivedRequiresContext(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKeyWithOptions(ctx, "k", KeyTypeAES256, KeyOptions{Derived: true})

	if _, err := e.EncryptWithContext(ctx, "k", []byte("x"), nil); !errors.Is(err, ErrContextRequired) {
		t.Fatalf("encrypt derived without context = %v, want ErrContextRequired", err)
	}
	// The plain Encrypt (nil context) is equivalent and must also be rejected.
	if _, err := e.Encrypt(ctx, "k", []byte("x")); !errors.Is(err, ErrContextRequired) {
		t.Fatalf("Encrypt on derived key = %v, want ErrContextRequired", err)
	}
}

func TestContextRejectedForNonDerivedKey(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKey(ctx, "plain")
	if _, err := e.EncryptWithContext(ctx, "plain", []byte("x"), []byte("ctx")); !errors.Is(err, ErrContextNotAllowed) {
		t.Fatalf("context on non-derived key = %v, want ErrContextNotAllowed", err)
	}
}

func TestConvergentDeterminism(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	info, err := e.CreateKeyWithOptions(ctx, "c", KeyTypeAES256, KeyOptions{Convergent: true})
	if err != nil {
		t.Fatalf("create convergent: %v", err)
	}
	if !info.Convergent || !info.Derived {
		t.Fatalf("convergent key should be derived+convergent: %+v", info)
	}

	dctx := []byte("ctx")
	pt := []byte("same-plaintext")
	a, _ := e.EncryptWithContext(ctx, "c", pt, dctx)
	b, _ := e.EncryptWithContext(ctx, "c", pt, dctx)
	if a != b {
		t.Fatalf("convergent encryption not deterministic:\n a=%s\n b=%s", a, b)
	}
	// Different context → different ciphertext.
	c, _ := e.EncryptWithContext(ctx, "c", pt, []byte("other"))
	if c == a {
		t.Fatal("different context produced identical ciphertext")
	}
	// Different plaintext → different ciphertext.
	d, _ := e.EncryptWithContext(ctx, "c", []byte("other-plaintext"), dctx)
	if d == a {
		t.Fatal("different plaintext produced identical ciphertext")
	}
	// And it still decrypts.
	got, err := e.DecryptWithContext(ctx, "c", a, dctx)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("convergent decrypt = %q, %v", got, err)
	}
}

func TestNonConvergentDerivedIsRandomized(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKeyWithOptions(ctx, "d", KeyTypeAES256, KeyOptions{Derived: true})
	dctx := []byte("ctx")
	a, _ := e.EncryptWithContext(ctx, "d", []byte("pt"), dctx)
	b, _ := e.EncryptWithContext(ctx, "d", []byte("pt"), dctx)
	if a == b {
		t.Fatal("non-convergent derived encryption should use a random nonce (got identical ciphertext)")
	}
}

func TestConvergentRequiresSymmetric(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKeyWithOptions(ctx, "sig", KeyTypeEd25519, KeyOptions{Derived: true}); !errors.Is(err, ErrKeyTypeMismatch) {
		t.Fatalf("derived signing key = %v, want ErrKeyTypeMismatch", err)
	}
}

func TestRewrapWithContextAfterRotate(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.CreateKeyWithOptions(ctx, "k", KeyTypeAES256, KeyOptions{Derived: true})
	dctx := []byte("ctx")
	old, _ := e.EncryptWithContext(ctx, "k", []byte("pt"), dctx)
	if _, err := e.Rotate(ctx, "k"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	rewrapped, err := e.RewrapWithContext(ctx, "k", old, dctx)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if rewrapped == old {
		t.Fatal("rewrap did not change the ciphertext (should move to the new version)")
	}
	got, err := e.DecryptWithContext(ctx, "k", rewrapped, dctx)
	if err != nil || !bytes.Equal(got, []byte("pt")) {
		t.Fatalf("decrypt rewrapped = %q, %v", got, err)
	}
}
