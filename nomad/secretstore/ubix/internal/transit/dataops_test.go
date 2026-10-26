package transit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRewrapUpgradesVersion(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "orders"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	plaintext := []byte("card-number-4242")
	ctV1, err := e.Encrypt(ctx, "orders", plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Rotate so the latest version advances, then rewrap the old ciphertext.
	if _, err := e.Rotate(ctx, "orders"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	ctV2, err := e.Rewrap(ctx, "orders", ctV1)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	if !strings.HasPrefix(ctV2, "ubix:v2:") {
		t.Fatalf("rewrapped ciphertext = %q, want ubix:v2: prefix", ctV2)
	}
	if ctV2 == ctV1 {
		t.Fatal("rewrap returned identical ciphertext")
	}

	// The rewrapped ciphertext still decrypts to the original plaintext.
	got, err := e.Decrypt(ctx, "orders", ctV2)
	if err != nil {
		t.Fatalf("Decrypt rewrapped: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip = %q, want %q", got, plaintext)
	}
}

func TestRewrapUnknownKey(t *testing.T) {
	e := newEngine()
	if _, err := e.Rewrap(context.Background(), "nope", "ubix:v1:AAAA"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Rewrap unknown key = %v, want ErrKeyNotFound", err)
	}
}

func TestGenerateDataKey(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "kek"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	for _, bits := range []int{128, 256, 512} {
		plaintext, wrapped, err := e.GenerateDataKey(ctx, "kek", bits)
		if err != nil {
			t.Fatalf("GenerateDataKey(%d): %v", bits, err)
		}
		if len(plaintext) != bits/8 {
			t.Fatalf("data key length = %d, want %d", len(plaintext), bits/8)
		}
		// The wrapped form decrypts back to the plaintext data key.
		got, err := e.Decrypt(ctx, "kek", wrapped)
		if err != nil {
			t.Fatalf("Decrypt wrapped data key: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("unwrapped data key != plaintext for %d bits", bits)
		}
	}
}

func TestGenerateDataKeyDistinct(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "kek"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	a, _, err := e.GenerateDataKey(ctx, "kek", 256)
	if err != nil {
		t.Fatalf("GenerateDataKey a: %v", err)
	}
	b, _, err := e.GenerateDataKey(ctx, "kek", 256)
	if err != nil {
		t.Fatalf("GenerateDataKey b: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two generated data keys were identical")
	}
}

func TestGenerateDataKeyInvalidBits(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.CreateKey(ctx, "kek"); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, _, err := e.GenerateDataKey(ctx, "kek", 300); !errors.Is(err, ErrInvalidDataKeyBits) {
		t.Fatalf("GenerateDataKey(300) = %v, want ErrInvalidDataKeyBits", err)
	}
}
