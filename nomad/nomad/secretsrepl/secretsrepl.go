// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package secretsrepl encrypts the values of the built-in secret store for
// their replication to another region (norca-doc
// content/docs/design/secrets-plan.md, "Multi-region operation").
//
// Every region has an X25519 replication key pair. The private key is kept
// in the region's secret store barrier, so it is protected by the region's
// seal like the secrets themselves; the public key is handed out to the
// servers of the authoritative region. The leader of the authoritative
// region encrypts each replicated value for the public key of the region
// that asked for it, in addition to the servers' mutual TLS: a value is in
// plaintext only in the memory of the two leaders.
//
// The scheme is an ECIES over X25519: a fresh ephemeral key pair per value,
// the X25519 shared secret expanded with HKDF-SHA256 (salted with both
// public keys) into an AES-256-GCM key, and the caller's associated data,
// which binds the ciphertext to the regions, the key, the secret and the
// version, authenticated by the GCM tag. Sealed values are never stored: a
// replica is written re-encrypted by the barrier of its own region.
package secretsrepl

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	// version is the first byte of a sealed value.
	version byte = 1

	// keySize is the size of X25519 keys.
	keySize = 32

	// hkdfInfo separates the derived keys from any other use of X25519.
	hkdfInfo = "norca secret replication v1"
)

// ErrDecrypt is returned for a sealed value that cannot be opened with the
// key and associated data given: another key, altered data or a value
// encrypted for another secret, version or region.
var ErrDecrypt = errors.New("secret replication: cannot decrypt the value")

// PrivateKey is the X25519 replication key of a region.
type PrivateKey struct {
	key *ecdh.PrivateKey
}

// GenerateKey returns a new random replication key.
func GenerateKey() (*PrivateKey, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &PrivateKey{key: k}, nil
}

// ParsePrivateKey returns the key of its Bytes.
func ParsePrivateKey(b []byte) (*PrivateKey, error) {
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("secret replication: invalid private key: %w", err)
	}
	return &PrivateKey{key: k}, nil
}

// Bytes returns the private key.
func (k *PrivateKey) Bytes() []byte { return k.key.Bytes() }

// PublicKey returns the public key.
func (k *PrivateKey) PublicKey() []byte { return k.key.PublicKey().Bytes() }

// ID returns the ID of the key pair, KeyID of its public key.
func (k *PrivateKey) ID() string { return KeyID(k.PublicKey()) }

// KeyID identifies a public key: a prefix of its SHA-256 hash.
func KeyID(public []byte) string {
	sum := sha256.Sum256(public)
	return "x25519:" + hex.EncodeToString(sum[:16])
}

// ValidatePublicKey checks a public key.
func ValidatePublicKey(public []byte) error {
	if _, err := ecdh.X25519().NewPublicKey(public); err != nil {
		return fmt.Errorf("secret replication: invalid public key: %w", err)
	}
	return nil
}

// AAD builds the associated data of a sealed value from fields, each
// prefixed with its length, so that no two field lists give the same data.
func AAD(fields ...string) []byte {
	n := 0
	for _, f := range fields {
		n += 4 + len(f)
	}
	out := make([]byte, 0, n)
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f)))
		out = append(out, f...)
	}
	return out
}

// Seal encrypts plaintext for the public key, bound to aad.
func Seal(public, plaintext, aad []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(public)
	if err != nil {
		return nil, fmt.Errorf("secret replication: invalid public key: %w", err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("secret replication: key agreement: %w", err)
	}
	ephPub := eph.PublicKey().Bytes()
	aead, err := newAEAD(shared, ephPub, public)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	out := make([]byte, 0, 1+keySize+len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, version)
	out = append(out, ephPub...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts a value sealed for the key's public key with the same aad.
func (k *PrivateKey) Open(sealed, aad []byte) ([]byte, error) {
	const nonceSize = 12
	if len(sealed) < 1+keySize+nonceSize || sealed[0] != version {
		return nil, ErrDecrypt
	}
	ephPub := sealed[1 : 1+keySize]
	eph, err := ecdh.X25519().NewPublicKey(ephPub)
	if err != nil {
		return nil, ErrDecrypt
	}
	shared, err := k.key.ECDH(eph)
	if err != nil {
		return nil, ErrDecrypt
	}
	aead, err := newAEAD(shared, ephPub, k.PublicKey())
	if err != nil {
		return nil, err
	}
	nonce := sealed[1+keySize : 1+keySize+nonceSize]
	plaintext, err := aead.Open(nil, nonce, sealed[1+keySize+nonceSize:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// newAEAD derives the AES-256-GCM cipher of a shared secret.
func newAEAD(shared, ephPub, recipientPub []byte) (cipher.AEAD, error) {
	salt := make([]byte, 0, 2*keySize)
	salt = append(salt, ephPub...)
	salt = append(salt, recipientPub...)
	key, err := hkdf.Key(sha256.New, shared, salt, hkdfInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
