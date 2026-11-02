package transit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHMACRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "sig"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	data := []byte("message-to-authenticate")

	for _, algo := range []string{"", "sha2-256", "sha2-384", "sha2-512"} {
		mac, err := e.HMAC(ctx, "sig", data, algo)
		if err != nil {
			t.Fatalf("HMAC(%q): %v", algo, err)
		}
		if !strings.HasPrefix(mac, "ubix:v1:") {
			t.Fatalf("hmac = %q, want ubix:v1: prefix", mac)
		}
		ok, err := e.VerifyHMAC(ctx, "sig", data, mac, algo)
		if err != nil {
			t.Fatalf("VerifyHMAC(%q): %v", algo, err)
		}
		if !ok {
			t.Fatalf("VerifyHMAC(%q) = false, want true", algo)
		}
	}
}

func TestHMACRejectsTamperedData(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "sig"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	mac, err := e.HMAC(ctx, "sig", []byte("original"), "")
	if err != nil {
		t.Fatalf("HMAC: %v", err)
	}
	ok, err := e.VerifyHMAC(ctx, "sig", []byte("tampered"), mac, "")
	if err != nil {
		t.Fatalf("VerifyHMAC: %v", err)
	}
	if ok {
		t.Fatal("VerifyHMAC accepted a tampered message")
	}
}

func TestHMACWrongAlgorithmFailsVerify(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "sig"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	mac, err := e.HMAC(ctx, "sig", []byte("data"), "sha2-256")
	if err != nil {
		t.Fatalf("HMAC: %v", err)
	}
	// Verifying the sha2-256 MAC as sha2-512 must not pass.
	ok, err := e.VerifyHMAC(ctx, "sig", []byte("data"), mac, "sha2-512")
	if err != nil {
		t.Fatalf("VerifyHMAC: %v", err)
	}
	if ok {
		t.Fatal("VerifyHMAC accepted a MAC under the wrong algorithm")
	}
}

func TestHMACRotationVersioning(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "sig"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	data := []byte("data")
	macV1, err := e.HMAC(ctx, "sig", data, "")
	if err != nil {
		t.Fatalf("HMAC v1: %v", err)
	}
	if _, err := e.Rotate(ctx, "sig"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	// The v1 MAC still verifies (its version is named in the string)...
	if ok, _ := e.VerifyHMAC(ctx, "sig", data, macV1, ""); !ok {
		t.Fatal("v1 MAC failed to verify after rotation")
	}
	// ...and a new MAC is bound to v2.
	macV2, err := e.HMAC(ctx, "sig", data, "")
	if err != nil {
		t.Fatalf("HMAC v2: %v", err)
	}
	if !strings.HasPrefix(macV2, "ubix:v2:") {
		t.Fatalf("post-rotation hmac = %q, want ubix:v2: prefix", macV2)
	}
	if macV1 == macV2 {
		t.Fatal("MAC did not change across key rotation")
	}
}

func TestHMACInvalidAlgorithm(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "sig"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := e.HMAC(ctx, "sig", []byte("x"), "md5"); !errors.Is(err, ErrInvalidAlgorithm) {
		t.Fatalf("HMAC(md5) = %v, want ErrInvalidAlgorithm", err)
	}
}

func TestVerifyHMACMalformed(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "sig"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := e.VerifyHMAC(ctx, "sig", []byte("x"), "not-a-mac", ""); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("VerifyHMAC(malformed) = %v, want ErrInvalidCiphertext", err)
	}
}
