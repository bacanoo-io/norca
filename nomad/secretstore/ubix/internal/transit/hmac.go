package transit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
)

// ErrInvalidAlgorithm is returned for an unsupported HMAC hash algorithm.
var ErrInvalidAlgorithm = errors.New("transit: unsupported algorithm")

// hmacHash maps an algorithm name to its hash constructor. The empty string
// defaults to sha2-256.
func hmacHash(algo string) (func() hash.Hash, error) {
	switch algo {
	case "", "sha2-256":
		return sha256.New, nil
	case "sha2-384":
		return sha512.New384, nil
	case "sha2-512":
		return sha512.New, nil
	default:
		return nil, ErrInvalidAlgorithm
	}
}

// HMAC computes an HMAC of data using the key's latest version as the secret and
// the named hash algorithm (sha2-256/384/512; default sha2-256). The result is a
// self-describing "ubix:v<N>:<base64>" string so [Engine.VerifyHMAC] can select
// the version that produced it.
func (e *Engine) HMAC(ctx context.Context, name string, data []byte, algo string) (string, error) {
	hfn, err := hmacHash(algo)
	if err != nil {
		return "", err
	}
	k, err := e.load(ctx, name)
	if err != nil {
		return "", err
	}
	if !isSymmetric(k.Type) {
		return "", ErrKeyTypeMismatch
	}
	mac := hmac.New(hfn, k.Versions[k.LatestVersion])
	mac.Write(data)
	return fmt.Sprintf("%sv%d:%s", cipherPrefix, k.LatestVersion, base64.StdEncoding.EncodeToString(mac.Sum(nil))), nil
}

// VerifyHMAC reports whether mac is a valid HMAC of data under the key version it
// names, using algo. The comparison is constant-time. A well-formed mac that
// simply doesn't match returns (false, nil); a malformed mac returns an error.
func (e *Engine) VerifyHMAC(ctx context.Context, name string, data []byte, mac, algo string) (bool, error) {
	version, want, err := parseCiphertext(mac)
	if err != nil {
		return false, err
	}
	hfn, err := hmacHash(algo)
	if err != nil {
		return false, err
	}
	k, err := e.load(ctx, name)
	if err != nil {
		return false, err
	}
	material, ok := k.Versions[version]
	if !ok {
		return false, nil
	}
	m := hmac.New(hfn, material)
	m.Write(data)
	return hmac.Equal(m.Sum(nil), want), nil
}
