package transit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var signingTypes = []string{KeyTypeECDSAP256, KeyTypeECDSAP384, KeyTypeECDSAP521, KeyTypeEd25519}

func TestSignVerifyRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, kt := range signingTypes {
		e := newEngine()
		info, err := e.CreateTypedKey(ctx, "signer", kt)
		if err != nil {
			t.Fatalf("%s CreateTypedKey: %v", kt, err)
		}
		if info.Type != kt {
			t.Fatalf("%s: KeyInfo.Type = %q", kt, info.Type)
		}
		if len(info.PublicKeys) != 1 || !strings.Contains(info.PublicKeys[1], "PUBLIC KEY") {
			t.Fatalf("%s: expected a PEM public key, got %v", kt, info.PublicKeys)
		}

		msg := []byte("release-artifact-v1.2.3")
		sig, err := e.Sign(ctx, "signer", msg, "")
		if err != nil {
			t.Fatalf("%s Sign: %v", kt, err)
		}
		if !strings.HasPrefix(sig, "ubix:v1:") {
			t.Fatalf("%s signature = %q, want ubix:v1: prefix", kt, sig)
		}
		ok, err := e.Verify(ctx, "signer", msg, sig, "")
		if err != nil {
			t.Fatalf("%s Verify: %v", kt, err)
		}
		if !ok {
			t.Fatalf("%s: valid signature did not verify", kt)
		}
		// A different message must not verify.
		if ok, _ := e.Verify(ctx, "signer", []byte("tampered"), sig, ""); ok {
			t.Fatalf("%s: tampered message verified", kt)
		}
	}
}

func TestSignRotationVersioning(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateTypedKey(ctx, "signer", KeyTypeECDSAP256); err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}
	msg := []byte("data")
	sigV1, err := e.Sign(ctx, "signer", msg, "")
	if err != nil {
		t.Fatalf("Sign v1: %v", err)
	}
	if _, err := e.Rotate(ctx, "signer"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	// The v1 signature still verifies against the retained v1 key.
	if ok, _ := e.Verify(ctx, "signer", msg, sigV1, ""); !ok {
		t.Fatal("v1 signature failed to verify after rotation")
	}
	sigV2, err := e.Sign(ctx, "signer", msg, "")
	if err != nil {
		t.Fatalf("Sign v2: %v", err)
	}
	if !strings.HasPrefix(sigV2, "ubix:v2:") {
		t.Fatalf("post-rotation signature = %q, want ubix:v2: prefix", sigV2)
	}
}

func TestSignVerifyCrossKeyFails(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateTypedKey(ctx, "a", KeyTypeECDSAP256); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := e.CreateTypedKey(ctx, "b", KeyTypeECDSAP256); err != nil {
		t.Fatalf("create b: %v", err)
	}
	msg := []byte("data")
	sig, err := e.Sign(ctx, "a", msg, "")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// A's signature must not verify under B's key.
	if ok, _ := e.Verify(ctx, "b", msg, sig, ""); ok {
		t.Fatal("signature from key a verified under key b")
	}
}

func TestSignOnSymmetricKeyRejected(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "sym"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := e.Sign(ctx, "sym", []byte("x"), ""); !errors.Is(err, ErrKeyTypeMismatch) {
		t.Fatalf("Sign on symmetric key = %v, want ErrKeyTypeMismatch", err)
	}
}

func TestEncryptOnSigningKeyRejected(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateTypedKey(ctx, "signer", KeyTypeEd25519); err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}
	if _, err := e.Encrypt(ctx, "signer", []byte("x")); !errors.Is(err, ErrKeyTypeMismatch) {
		t.Fatalf("Encrypt on signing key = %v, want ErrKeyTypeMismatch", err)
	}
}

func TestCreateInvalidKeyType(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateTypedKey(ctx, "x", "rsa-9999"); !errors.Is(err, ErrInvalidKeyType) {
		t.Fatalf("CreateTypedKey(bad) = %v, want ErrInvalidKeyType", err)
	}
}

func TestECDSAAlgorithmVariants(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateTypedKey(ctx, "signer", KeyTypeECDSAP256); err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}
	msg := []byte("data")
	for _, algo := range []string{"sha2-256", "sha2-384", "sha2-512"} {
		sig, err := e.Sign(ctx, "signer", msg, algo)
		if err != nil {
			t.Fatalf("Sign(%s): %v", algo, err)
		}
		if ok, _ := e.Verify(ctx, "signer", msg, sig, algo); !ok {
			t.Fatalf("Verify(%s) failed", algo)
		}
	}
}
