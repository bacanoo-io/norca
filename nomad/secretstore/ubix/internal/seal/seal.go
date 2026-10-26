// Package seal abstracts how an auto-unsealing vault protects its master key at
// rest. The Shamir path (operator-supplied key shares) lives in the core; a Seal
// covers the automatic modes:
//
//   - StaticKEK wraps the master key with a locally-held key-encryption key.
//   - Transit wraps it via a remote Vault-compatible Transit engine, so the
//     wrapping key never leaves that vault (no KEK on this host).
//
// Wrap output is opaque to the core, which just stores and later Unwraps it.
package seal

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// Seal wraps and unwraps the master key for auto-unseal.
type Seal interface {
	// Type is the seal type recorded in the seal config ("auto", "transit").
	Type() string
	// Wrap encrypts plaintext for storage.
	Wrap(ctx context.Context, plaintext []byte) ([]byte, error)
	// Unwrap reverses Wrap.
	Unwrap(ctx context.Context, wrapped []byte) ([]byte, error)
}

// kekSize is the required key-encryption-key length for StaticKEK (AES-256).
const kekSize = 32

// StaticKEK wraps the master key with a locally-held 32-byte KEK using
// AES-256-GCM. This is the seal behind the -auto-unseal-key flag.
type StaticKEK struct {
	kek []byte
}

// NewStaticKEK returns a static-KEK seal. The key length is validated lazily on
// first Wrap/Unwrap so this never fails at construction.
func NewStaticKEK(kek []byte) *StaticKEK {
	return &StaticKEK{kek: kek}
}

// Type implements [Seal].
func (s *StaticKEK) Type() string { return "auto" }

// Wrap encrypts plaintext as nonce || ciphertext+tag.
func (s *StaticKEK) Wrap(_ context.Context, plaintext []byte) ([]byte, error) {
	aead, err := s.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: wrap nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Unwrap reverses Wrap; a wrong KEK fails GCM authentication.
func (s *StaticKEK) Unwrap(_ context.Context, wrapped []byte) ([]byte, error) {
	aead, err := s.aead()
	if err != nil {
		return nil, err
	}
	if len(wrapped) < aead.NonceSize() {
		return nil, fmt.Errorf("seal: wrapped master key malformed")
	}
	nonce, ct := wrapped[:aead.NonceSize()], wrapped[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("seal: auto-unseal key incorrect: %w", err)
	}
	return plaintext, nil
}

func (s *StaticKEK) aead() (cipher.AEAD, error) {
	if len(s.kek) != kekSize {
		return nil, fmt.Errorf("seal: auto-unseal key must be %d bytes", kekSize)
	}
	block, err := aes.NewCipher(s.kek)
	if err != nil {
		return nil, fmt.Errorf("seal: cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
