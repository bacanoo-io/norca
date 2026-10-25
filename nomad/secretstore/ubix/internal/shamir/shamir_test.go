package shamir

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

// randomSecret returns n cryptographically random bytes.
func randomSecret(t *testing.T, n int) []byte {
	t.Helper()
	s := make([]byte, n)
	if _, err := rand.Read(s); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return s
}

// --- Field arithmetic ---

// TestGFMulKnownVectors checks multiplication against the authoritative examples
// from FIPS-197 (the AES specification), which fixes the field and reduction
// polynomial we use.
func TestGFMulKnownVectors(t *testing.T) {
	cases := []struct{ a, b, want byte }{
		{0x57, 0x83, 0xc1}, // FIPS-197 §4.2
		{0x57, 0x13, 0xfe}, // FIPS-197 §4.2.1
		{0x01, 0x9a, 0x9a}, // identity
		{0x00, 0xff, 0x00}, // zero
	}
	for _, c := range cases {
		if got := gfMul(c.a, c.b); got != c.want {
			t.Errorf("gfMul(%#x,%#x) = %#x, want %#x", c.a, c.b, got, c.want)
		}
		if got := gfMul(c.b, c.a); got != c.want {
			t.Errorf("gfMul not commutative for %#x,%#x", c.a, c.b)
		}
	}
}

// TestGFInverse verifies a * a^-1 == 1 for every non-zero element.
func TestGFInverse(t *testing.T) {
	for a := 1; a <= 255; a++ {
		if got := gfMul(byte(a), gfInverse(byte(a))); got != 1 {
			t.Fatalf("a*inv(a) for a=%#x = %#x, want 1", a, got)
		}
	}
}

// --- Split / Combine ---

func TestSplitCombineRoundTrip(t *testing.T) {
	configs := []struct{ parts, threshold int }{
		{2, 2}, {3, 2}, {5, 3}, {10, 7}, {255, 2}, {255, 255},
	}
	secret := randomSecret(t, 32)
	for _, c := range configs {
		shares, err := Split(secret, c.parts, c.threshold)
		if err != nil {
			t.Fatalf("Split(%d,%d): %v", c.parts, c.threshold, err)
		}
		if len(shares) != c.parts {
			t.Fatalf("Split(%d,%d): got %d shares", c.parts, c.threshold, len(shares))
		}
		// Combine the first `threshold` shares.
		got, err := Combine(shares[:c.threshold])
		if err != nil {
			t.Fatalf("Combine(%d,%d): %v", c.parts, c.threshold, err)
		}
		if !bytes.Equal(got, secret) {
			t.Fatalf("Combine(%d,%d) did not recover secret", c.parts, c.threshold)
		}
	}
}

// TestEveryThresholdSubsetRecovers checks that *any* threshold subset works, and
// they all agree.
func TestEveryThresholdSubsetRecovers(t *testing.T) {
	secret := randomSecret(t, 16)
	const parts, threshold = 6, 3
	shares, err := Split(secret, parts, threshold)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	for _, combo := range combinations(parts, threshold) {
		subset := make([][]byte, 0, threshold)
		for _, i := range combo {
			subset = append(subset, shares[i])
		}
		got, err := Combine(subset)
		if err != nil {
			t.Fatalf("Combine(%v): %v", combo, err)
		}
		if !bytes.Equal(got, secret) {
			t.Fatalf("subset %v did not recover secret", combo)
		}
	}
}

// TestFewerThanThresholdDoesNotRecover confirms that threshold-1 shares produce
// something other than the secret (the security property, observationally).
func TestFewerThanThresholdDoesNotRecover(t *testing.T) {
	secret := randomSecret(t, 32)
	const parts, threshold = 5, 3
	shares, err := Split(secret, parts, threshold)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	got, err := Combine(shares[:threshold-1]) // only 2 of 3
	if err != nil {
		t.Fatalf("Combine: %v", err)
	}
	if bytes.Equal(got, secret) {
		t.Fatal("threshold-1 shares recovered the secret (should be astronomically unlikely)")
	}
}

func TestSingleByteAndLargeSecrets(t *testing.T) {
	for _, n := range []int{1, 2, 32, 1024} {
		secret := randomSecret(t, n)
		shares, err := Split(secret, 4, 2)
		if err != nil {
			t.Fatalf("Split(len=%d): %v", n, err)
		}
		if got := len(shares[0]); got != n+1 {
			t.Fatalf("share length for secret len %d = %d, want %d", n, got, n+1)
		}
		out, err := Combine(shares[:2])
		if err != nil {
			t.Fatalf("Combine(len=%d): %v", n, err)
		}
		if !bytes.Equal(out, secret) {
			t.Fatalf("round-trip failed for secret len %d", n)
		}
	}
}

func TestShareCoordinatesDistinctAndNonZero(t *testing.T) {
	shares, err := Split(randomSecret(t, 8), 20, 5)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	seen := map[byte]bool{}
	for _, s := range shares {
		x := s[len(s)-1]
		if x == 0 {
			t.Fatal("share has zero x-coordinate")
		}
		if seen[x] {
			t.Fatalf("duplicate x-coordinate %#x", x)
		}
		seen[x] = true
	}
}

func TestSplitErrors(t *testing.T) {
	secret := randomSecret(t, 8)
	cases := []struct {
		name             string
		secret           []byte
		parts, threshold int
		want             error
	}{
		{"empty", nil, 3, 2, ErrEmptySecret},
		{"parts too low", secret, 1, 1, ErrPartsRange},
		{"parts too high", secret, 256, 2, ErrPartsRange},
		{"threshold too low", secret, 3, 1, ErrThresholdRange},
		{"threshold above parts", secret, 3, 4, ErrThresholdRange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Split(c.secret, c.parts, c.threshold); !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
		})
	}
}

func TestCombineErrors(t *testing.T) {
	shares, err := Split(randomSecret(t, 8), 3, 2)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}

	if _, err := Combine(shares[:1]); !errors.Is(err, ErrTooFewShares) {
		t.Errorf("too few shares: want ErrTooFewShares, got %v", err)
	}

	mismatched := [][]byte{shares[0], shares[1][:len(shares[1])-1]}
	if _, err := Combine(mismatched); !errors.Is(err, ErrShareLength) {
		t.Errorf("mismatched length: want ErrShareLength, got %v", err)
	}

	dup := [][]byte{append([]byte(nil), shares[0]...), append([]byte(nil), shares[0]...)}
	if _, err := Combine(dup); !errors.Is(err, ErrDuplicateShares) {
		t.Errorf("duplicate x: want ErrDuplicateShares, got %v", err)
	}
}

// combinations returns all k-index combinations of [0,n).
func combinations(n, k int) [][]int {
	var res [][]int
	idx := make([]int, k)
	var rec func(start, depth int)
	rec = func(start, depth int) {
		if depth == k {
			res = append(res, append([]int(nil), idx...))
			return
		}
		for i := start; i < n; i++ {
			idx[depth] = i
			rec(i+1, depth+1)
		}
	}
	rec(0, 0)
	return res
}
