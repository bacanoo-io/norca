// Package shamir implements Shamir's Secret Sharing over GF(2^8).
//
// A secret (e.g. the barrier master key) is split into N shares such that any T
// of them reconstruct it and any T-1 reveal nothing (docs/DESIGN.md §3.1). Each
// share is len(secret)+1 bytes: one y-value per secret byte plus a trailing
// x-coordinate (the evaluation point).
//
// Implementing a cryptographic construction in-house is a deliberate, narrow
// exception to the "no hand-rolled cryptography" rule (docs/DECISIONS.md D-009):
// Shamir is a secret-sharing scheme, not a cipher, and the ciphers/primitives it
// would otherwise need (AES, GCM, hashes) remain standard-library only. The field
// arithmetic below is written to be constant-time with respect to its byte
// operands — no data-dependent branches and no table lookups — to avoid timing
// side channels, and is covered by known AES-field test vectors plus round-trip
// property tests.
package shamir

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
)

// Errors returned by Split and Combine.
var (
	ErrEmptySecret     = errors.New("shamir: secret must not be empty")
	ErrThresholdRange  = errors.New("shamir: threshold must be >= 2 and <= parts")
	ErrPartsRange      = errors.New("shamir: parts must be >= 2 and <= 255")
	ErrTooFewShares    = errors.New("shamir: need at least 2 shares to combine")
	ErrShareLength     = errors.New("shamir: shares must be the same length and >= 2 bytes")
	ErrDuplicateShares = errors.New("shamir: shares have duplicate or zero x-coordinates")
)

// Split divides secret into parts shares, any threshold of which reconstruct it.
// parts must be in [2,255] and threshold in [2,parts].
func Split(secret []byte, parts, threshold int) ([][]byte, error) {
	if len(secret) == 0 {
		return nil, ErrEmptySecret
	}
	if parts < 2 || parts > 255 {
		return nil, ErrPartsRange
	}
	if threshold < 2 || threshold > parts {
		return nil, ErrThresholdRange
	}

	xCoords, err := randomCoordinates(parts)
	if err != nil {
		return nil, err
	}

	shares := make([][]byte, parts)
	for i := range shares {
		shares[i] = make([]byte, len(secret)+1)
		shares[i][len(secret)] = xCoords[i] // trailing byte is the x-coordinate
	}

	// For each secret byte, build a random degree (threshold-1) polynomial whose
	// constant term is that byte, then sample it at each share's x-coordinate.
	for idx, b := range secret {
		coeffs, err := randomPolynomial(threshold-1, b)
		if err != nil {
			return nil, err
		}
		for i := range shares {
			shares[i][idx] = evaluate(coeffs, xCoords[i])
		}
	}
	return shares, nil
}

// Combine reconstructs a secret from shares produced by Split. It needs at least
// the original threshold of shares; fewer yields an incorrect result, more is
// fine. Providing fewer than threshold shares does not error (the scheme cannot
// detect the shortfall) — it simply does not recover the secret.
func Combine(shares [][]byte) ([]byte, error) {
	if len(shares) < 2 {
		return nil, ErrTooFewShares
	}
	shareLen := len(shares[0])
	if shareLen < 2 {
		return nil, ErrShareLength
	}
	for _, s := range shares {
		if len(s) != shareLen {
			return nil, ErrShareLength
		}
	}

	secretLen := shareLen - 1
	xs := make([]byte, len(shares))
	seen := make(map[byte]struct{}, len(shares))
	for i, s := range shares {
		x := s[secretLen]
		if x == 0 {
			return nil, ErrDuplicateShares
		}
		if _, dup := seen[x]; dup {
			return nil, ErrDuplicateShares
		}
		seen[x] = struct{}{}
		xs[i] = x
	}

	secret := make([]byte, secretLen)
	ys := make([]byte, len(shares))
	for idx := 0; idx < secretLen; idx++ {
		for i, s := range shares {
			ys[i] = s[idx]
		}
		secret[idx] = interpolate(xs, ys)
	}
	return secret, nil
}

// randomCoordinates returns n distinct non-zero x-coordinates in [1,255], chosen
// by a crypto/rand Fisher-Yates shuffle so share order carries no information.
func randomCoordinates(n int) ([]byte, error) {
	all := make([]byte, 255)
	for i := range all {
		all[i] = byte(i + 1) // 1..255
	}
	for i := 0; i < n; i++ {
		j, err := randIntn(255 - i)
		if err != nil {
			return nil, err
		}
		j += i
		all[i], all[j] = all[j], all[i]
	}
	return all[:n], nil
}

// randomPolynomial returns degree+1 coefficients with the given intercept
// (coefficient 0) and cryptographically random higher-degree coefficients.
func randomPolynomial(degree int, intercept byte) ([]byte, error) {
	coeffs := make([]byte, degree+1)
	coeffs[0] = intercept
	if degree > 0 {
		if _, err := rand.Read(coeffs[1:]); err != nil {
			return nil, fmt.Errorf("shamir: random coefficients: %w", err)
		}
	}
	return coeffs, nil
}

// randIntn returns a uniform random int in [0,bound) using crypto/rand.
func randIntn(bound int) (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(bound)))
	if err != nil {
		return 0, fmt.Errorf("shamir: random index: %w", err)
	}
	return int(n.Int64()), nil
}

// evaluate computes the polynomial (lowest-degree coefficient first) at x, using
// Horner's method in GF(2^8).
func evaluate(coeffs []byte, x byte) byte {
	out := coeffs[len(coeffs)-1]
	for i := len(coeffs) - 2; i >= 0; i-- {
		out = gfAdd(gfMul(out, x), coeffs[i])
	}
	return out
}

// interpolate reconstructs f(0) via Lagrange interpolation over the given points.
func interpolate(xs, ys []byte) byte {
	var result byte
	for i := range xs {
		// Lagrange basis at 0: L_i(0) = prod_{j!=i} x_j / (x_i - x_j).
		// In GF(2^8) subtraction is XOR and 0 - x_j = x_j.
		var num, den byte = 1, 1
		for j := range xs {
			if i == j {
				continue
			}
			num = gfMul(num, xs[j])
			den = gfMul(den, gfAdd(xs[i], xs[j]))
		}
		li := gfMul(num, gfInverse(den))
		result = gfAdd(result, gfMul(ys[i], li))
	}
	return result
}

// --- GF(2^8) arithmetic (Rijndael field, reduction polynomial 0x11b) ---
//
// All operations are constant-time with respect to their byte operands: no
// data-dependent branches and no table lookups.

// gfAdd is addition in GF(2^8): XOR.
func gfAdd(a, b byte) byte { return a ^ b }

// gfMul multiplies two field elements (Russian-peasant, branch-free reduction).
func gfMul(a, b byte) byte {
	var p byte
	for i := 0; i < 8; i++ {
		p ^= a & (byte(0) - (b & 1)) // add a to p iff low bit of b is set
		hi := byte(0) - (a >> 7)     // 0xFF iff high bit of a is set
		a <<= 1
		a ^= 0x1b & hi // reduce modulo the field polynomial
		b >>= 1
	}
	return p
}

// gfInverse returns the multiplicative inverse of a (a^254, since a^255 == 1 for
// a != 0). The exponent is a fixed constant, so the branch pattern does not
// depend on secret data. gfInverse(0) is 0 and is never used (denominators are
// differences of distinct x-coordinates, hence non-zero).
func gfInverse(a byte) byte {
	var result byte = 1
	b := a
	for exp := 254; exp > 0; exp >>= 1 {
		if exp&1 == 1 {
			result = gfMul(result, b)
		}
		b = gfMul(b, b)
	}
	return result
}
