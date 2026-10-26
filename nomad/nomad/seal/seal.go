// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package seal protects the key material of a Norca server at rest
// (docs/design/secrets-plan.md, "Key protection (seal)").
//
// # What the seal protects
//
// A server holds two kinds of keys that must not be usable from its disk or
// from a backup alone:
//
//   - The root keys of the keyring (Variables, workload identities, the mTLS
//     CA key), one file per key in the server's keystore directory. Each file
//     holds the root key encrypted with a random key-encrypting key (KEK);
//     the KEK is wrapped by every seal of the region (structs.SealWrappedKey).
//   - The master key of the built-in secret store core (uBixVault, see
//     nomad/secretstore). The core runs in its auto-unseal mode with
//     [CoreSeal], so the master key is wrapped by the same seals and stored
//     in Raft (the core's "core/auto-master" entry); the core's barrier key
//     and data stay encrypted under it as uBixVault designed.
//
// The two are siblings under the seal, not nested: neither key protects the
// other. Unsealing a server therefore unlocks both at once: the keystore is
// unwrapped and the core is auto-unsealed with the same seals, and with the
// aead seal both happen automatically at start, as in Nomad.
//
// # Seals
//
// A [Seal] wraps and unwraps a key. Two are implemented:
//
//   - [AEAD] (structs.SealTypeAEAD) keeps Nomad's behaviour: the KEK is
//     stored in the clear next to the key it protects (in the keystore it is
//     written in Nomad's own file format). It protects nothing; it is the
//     default for development and for migrating existing clusters.
//   - [Shares] (structs.SealTypeShares) wraps with a random 256-bit unseal key
//     that is split into Shamir shares (uBixVault's shamir package) when it is
//     generated and then forgotten. A started server is sealed until a
//     threshold of shares reconstructs the key; a key check stored in Raft
//     (structs.SealInfo.KeyCheck) verifies the result.
//
// The KMS and transit seals of the design (awskms, gcpckms, scwkms, transit)
// implement the same interface: a wrap records the seal type and the seal's
// key ID next to its ciphertext, so they plug in without changing any stored
// data. A key may carry wraps of two seals (the optional second seal); either
// unwraps it.
package seal

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/hashicorp/nomad/nomad/structs"
)

// KeySize is the size of the keys the seals use and wrap (AES-256).
const KeySize = 32

var (
	// ErrKeyMismatch is returned when a wrap was made with another key of
	// the same seal type (for example the shares of an earlier rewrap).
	ErrKeyMismatch = errors.New("seal: the key was wrapped with a different seal key")

	// ErrNoWrap is returned when a key carries no wrap of any known seal.
	ErrNoWrap = errors.New("seal: the key has no wrap of a configured seal")
)

// Seal wraps and unwraps keys.
type Seal interface {
	// Type is the seal type recorded in every wrap.
	Type() string

	// KeyID identifies the key of the seal, recorded in every wrap. It is
	// empty for seals without a key of their own (aead).
	KeyID() string

	// Available reports whether the seal can wrap and unwrap now; a shares
	// seal is unavailable until it is unsealed.
	Available() bool

	// Wrap encrypts a key. aad binds the wrap to its use: the same aad must
	// be given to Unwrap.
	Wrap(ctx context.Context, plaintext, aad []byte) (*structs.SealWrappedKey, error)

	// Unwrap decrypts a wrap made by this seal.
	Unwrap(ctx context.Context, w *structs.SealWrappedKey, aad []byte) ([]byte, error)
}

// WrapAll wraps a key with every seal. All seals must be available.
func WrapAll(ctx context.Context, seals []Seal, plaintext, aad []byte) ([]*structs.SealWrappedKey, error) {
	if len(seals) == 0 {
		return nil, fmt.Errorf("%w: no seal is available", structs.ErrSealed)
	}
	out := make([]*structs.SealWrappedKey, 0, len(seals))
	for _, s := range seals {
		if !s.Available() {
			return nil, fmt.Errorf("%w: the %s seal is not unsealed", structs.ErrSealed, s.Type())
		}
		w, err := s.Wrap(ctx, plaintext, aad)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// UnwrapAny unwraps a key with the first wrap whose seal lookup returns an
// available seal. It returns an error wrapping structs.ErrSealed if a wrap
// has a seal that is not available and no wrap could be unwrapped.
func UnwrapAny(ctx context.Context, lookup func(*structs.SealWrappedKey) Seal, wraps []*structs.SealWrappedKey, aad []byte) ([]byte, error) {
	var sealed, lastErr error
	for _, w := range wraps {
		if w == nil {
			continue
		}
		s := lookup(w)
		if s == nil {
			continue
		}
		if !s.Available() {
			sealed = fmt.Errorf("%w: the %s seal is not unsealed", structs.ErrSealed, s.Type())
			continue
		}
		key, err := s.Unwrap(ctx, w, aad)
		if err == nil {
			return key, nil
		}
		lastErr = err
	}
	switch {
	case lastErr != nil:
		return nil, lastErr
	case sealed != nil:
		return nil, sealed
	}
	return nil, ErrNoWrap
}

// AEAD is the aead seal: every wrap carries its own random key-encrypting
// key in the clear. It is always available.
type AEAD struct{}

var _ Seal = AEAD{}

// Type implements Seal.
func (AEAD) Type() string { return structs.SealTypeAEAD }

// KeyID implements Seal.
func (AEAD) KeyID() string { return "" }

// Available implements Seal.
func (AEAD) Available() bool { return true }

// Wrap implements Seal.
func (AEAD) Wrap(_ context.Context, plaintext, aad []byte) (*structs.SealWrappedKey, error) {
	kek, err := RandomKey()
	if err != nil {
		return nil, err
	}
	ct, err := gcmSeal(kek, plaintext, aad)
	if err != nil {
		return nil, err
	}
	return &structs.SealWrappedKey{Seal: structs.SealTypeAEAD, Ciphertext: ct, KEK: kek}, nil
}

// Unwrap implements Seal.
func (AEAD) Unwrap(_ context.Context, w *structs.SealWrappedKey, aad []byte) ([]byte, error) {
	if w == nil || w.Seal != structs.SealTypeAEAD {
		return nil, errors.New("seal: not an aead wrap")
	}
	return gcmOpen(w.KEK, w.Ciphertext, aad)
}

// RandomKey returns a new random key of KeySize bytes.
func RandomKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("seal: failed to generate a key: %w", err)
	}
	return key, nil
}

// gcmSeal encrypts with AES-256-GCM and returns nonce || ciphertext.
func gcmSeal(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: failed to generate a nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

// gcmOpen reverses gcmSeal.
func gcmOpen(key, ciphertext, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aead.NonceSize() {
		return nil, errors.New("seal: malformed wrapped key")
	}
	nonce, ct := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, errors.New("seal: failed to unwrap the key: wrong seal key or modified data")
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("seal: keys must be %d bytes", KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return cipher.NewGCM(block)
}

// zero overwrites b (best-effort key hygiene).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
