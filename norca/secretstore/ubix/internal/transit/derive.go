package transit

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Context-derivation errors.
var (
	// ErrContextRequired is returned when a derived key is used without a context.
	ErrContextRequired = fmt.Errorf("transit: this key requires a derivation context")
	// ErrContextNotAllowed is returned when a context is supplied for a
	// non-derived key.
	ErrContextNotAllowed = fmt.Errorf("transit: this key does not take a derivation context")
)

// HKDF info labels — domain-separate the two derivations from one key.
const (
	deriveKeyInfo   = "ubixvault/transit/derived-key"
	deriveNonceInfo = "ubixvault/transit/convergent-nonce"
)

// opKey returns the effective symmetric key for an operation: the version's
// material directly, or — for a derived key — an HKDF-SHA256 subkey bound to the
// context. It also enforces the context rules.
func opKey(k *keyData, material, dctx []byte) ([]byte, error) {
	if k.Derived {
		if len(dctx) == 0 {
			return nil, ErrContextRequired
		}
		return hkdf.Key(sha256.New, material, dctx, deriveKeyInfo, 32)
	}
	if len(dctx) != 0 {
		return nil, ErrContextNotAllowed
	}
	return material, nil
}

// aad binds the key name and (for derived keys) the context into the AEAD's
// additional authenticated data, so a ciphertext only opens under the same name
// and context it was sealed with.
func aad(name string, dctx []byte) []byte {
	if len(dctx) == 0 {
		return []byte(name)
	}
	return append([]byte(name+"\x00"), dctx...)
}

// convergentNonce derives a deterministic nonce from the plaintext and context,
// so identical (plaintext, context) under the same key version encrypt to
// identical ciphertext. It is a keyed HMAC, so the plaintext is not recoverable
// from the nonce.
func convergentNonce(material, dctx, plaintext []byte, size int) []byte {
	nonceKey, _ := hkdf.Key(sha256.New, material, dctx, deriveNonceInfo, 32)
	mac := hmac.New(sha256.New, nonceKey)
	mac.Write(plaintext)
	return mac.Sum(nil)[:size]
}

// EncryptWithContext encrypts plaintext under the key's latest version. For a
// derived key the context selects the subkey (and, for a convergent key, makes
// the ciphertext deterministic); for a non-derived key the context must be empty.
func (e *Engine) EncryptWithContext(ctx context.Context, name string, plaintext, dctx []byte) (string, error) {
	k, err := e.load(ctx, name)
	if err != nil {
		return "", err
	}
	if !isSymmetric(k.Type) {
		return "", ErrKeyTypeMismatch
	}
	material := k.Versions[k.LatestVersion]
	opk, err := opKey(k, material, dctx)
	if err != nil {
		return "", err
	}
	aead, err := newAEAD(opk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if k.Convergent {
		nonce = convergentNonce(material, dctx, plaintext, aead.NonceSize())
	} else if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("transit: nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, plaintext, aad(name, dctx))
	return fmt.Sprintf("%sv%d:%s", cipherPrefix, k.LatestVersion, base64.StdEncoding.EncodeToString(sealed)), nil
}

// DecryptWithContext reverses EncryptWithContext, selecting the key version named
// in the ciphertext. The same context used to encrypt must be supplied.
func (e *Engine) DecryptWithContext(ctx context.Context, name, ciphertext string, dctx []byte) ([]byte, error) {
	version, blob, err := parseCiphertext(ciphertext)
	if err != nil {
		return nil, err
	}
	k, err := e.load(ctx, name)
	if err != nil {
		return nil, err
	}
	if !isSymmetric(k.Type) {
		return nil, ErrKeyTypeMismatch
	}
	material, ok := k.Versions[version]
	if !ok {
		return nil, ErrInvalidCiphertext
	}
	opk, err := opKey(k, material, dctx)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(opk)
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize() {
		return nil, ErrInvalidCiphertext
	}
	nonce, ct := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ct, aad(name, dctx))
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plaintext, nil
}

// RewrapWithContext is [Engine.Rewrap] for a derived key: it decrypts and
// re-encrypts under the latest version using the given context.
func (e *Engine) RewrapWithContext(ctx context.Context, name, ciphertext string, dctx []byte) (string, error) {
	plaintext, err := e.DecryptWithContext(ctx, name, ciphertext, dctx)
	if err != nil {
		return "", err
	}
	return e.EncryptWithContext(ctx, name, plaintext, dctx)
}
