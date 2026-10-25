package shamir

import (
	"bytes"
	"testing"
)

// FuzzSplitCombine is a property test for the in-house GF(2^8) Shamir scheme:
// for any secret and any valid (parts, threshold), splitting and then combining
// must recover the secret exactly — from the full set and from any threshold
// subset. This exercises the field arithmetic and Lagrange interpolation across
// randomized inputs, complementing the fixed-vector unit tests.
func FuzzSplitCombine(f *testing.F) {
	f.Add([]byte("secret"), uint8(5), uint8(3))
	f.Add([]byte{0}, uint8(2), uint8(2))
	f.Add([]byte{0xff, 0x00, 0x7f}, uint8(255), uint8(2))
	f.Add(bytes.Repeat([]byte{0xab}, 64), uint8(10), uint8(7))

	f.Fuzz(func(t *testing.T, secret []byte, p, th uint8) {
		// Split requires a non-empty secret; cap the length so 255-way splits of a
		// large secret don't dominate fuzz time.
		if len(secret) == 0 || len(secret) > 512 {
			return
		}
		// Normalize into the valid ranges: parts 2..255, threshold 2..parts.
		parts := int(p)%254 + 2
		threshold := int(th)%(parts-1) + 2

		shares, err := Split(secret, parts, threshold)
		if err != nil {
			t.Fatalf("Split(len=%d, parts=%d, threshold=%d): %v", len(secret), parts, threshold, err)
		}
		if len(shares) != parts {
			t.Fatalf("got %d shares, want %d", len(shares), parts)
		}

		// The full set recovers the secret.
		if got, err := Combine(shares); err != nil || !bytes.Equal(got, secret) {
			t.Fatalf("Combine(all) = %x, %v; want %x", got, err, secret)
		}
		// The first threshold shares recover it.
		if got, err := Combine(shares[:threshold]); err != nil || !bytes.Equal(got, secret) {
			t.Fatalf("Combine(first %d) = %x, %v; want %x", threshold, got, err, secret)
		}
		// A different threshold subset (the last `threshold` shares) also recovers it.
		if got, err := Combine(shares[parts-threshold:]); err != nil || !bytes.Equal(got, secret) {
			t.Fatalf("Combine(last %d) = %x, %v; want %x", threshold, got, err, secret)
		}
	})
}
