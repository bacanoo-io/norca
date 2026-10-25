// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package signing implements key-based signatures of pack artifacts in the
// format used by cosign, so that signatures created by "norca pack sign" can be
// checked with "cosign verify --key" and vice versa.
//
// The formats are:
//
//   - Private keys: PEM block "ENCRYPTED SIGSTORE PRIVATE KEY" (cosign also
//     accepts and writes "ENCRYPTED COSIGN PRIVATE KEY"), whose body is a JSON
//     document with the scrypt key derivation parameters, the
//     nacl/secretbox nonce and the encrypted PKCS #8 DER private key.
//   - Public keys: PEM block "PUBLIC KEY" with a PKIX DER public key.
//   - Signatures: the base64 encoded ASN.1 ECDSA signature of the SHA-256 of
//     the cosign "simple signing" payload (see payload.go).
package signing

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

const (
	// PEMTypeEncryptedPrivateKey is the PEM type written by current cosign
	// versions and by this package.
	PEMTypeEncryptedPrivateKey = "ENCRYPTED SIGSTORE PRIVATE KEY"

	// PEMTypeEncryptedCosignPrivateKey is the PEM type of older cosign keys.
	PEMTypeEncryptedCosignPrivateKey = "ENCRYPTED COSIGN PRIVATE KEY"

	// PEMTypePublicKey is the PEM type of public keys.
	PEMTypePublicKey = "PUBLIC KEY"

	kdfScrypt       = "scrypt"
	cipherSecretBox = "nacl/secretbox"
	saltSize        = 32
	nonceSize       = 24
	boxKeySize      = 32
)

// scryptParams are the key derivation parameters stored with an encrypted key.
type scryptParams struct {
	N int `json:"N"`
	R int `json:"r"`
	P int `json:"p"`
}

// The parameter sets understood by cosign. Keys are written with the first
// one, which every cosign version accepts; reading accepts all of them, which
// bounds the memory an attacker supplied key file can make scrypt allocate.
var acceptedScryptParams = []scryptParams{
	{N: 32768, R: 8, P: 1},
	{N: 65536, R: 8, P: 1},
	{N: 131072, R: 8, P: 1},
}

type encryptedKey struct {
	KDF struct {
		Name   string       `json:"name"`
		Params scryptParams `json:"params"`
		Salt   []byte       `json:"salt"`
	} `json:"kdf"`
	Cipher struct {
		Name  string `json:"name"`
		Nonce []byte `json:"nonce"`
	} `json:"cipher"`
	Ciphertext []byte `json:"ciphertext"`
}

// KeyPair is a generated key pair in its encoded form.
type KeyPair struct {
	// PrivateKeyPEM is the encrypted private key.
	PrivateKeyPEM []byte
	// PublicKeyPEM is the public key.
	PublicKeyPEM []byte
}

// GenerateKeyPair creates a new ECDSA P-256 key pair, the key type cosign
// generates, and encrypts the private key with password.
func GenerateKeyPair(password []byte) (*KeyPair, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	privPEM, err := EncryptPrivateKey(priv, password)
	if err != nil {
		return nil, err
	}
	pubPEM, err := MarshalPublicKey(priv.Public())
	if err != nil {
		return nil, err
	}
	return &KeyPair{PrivateKeyPEM: privPEM, PublicKeyPEM: pubPEM}, nil
}

// EncryptPrivateKey encodes key as an encrypted cosign private key.
func EncryptPrivateKey(key crypto.Signer, password []byte) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}

	var enc encryptedKey
	enc.KDF.Name = kdfScrypt
	enc.KDF.Params = acceptedScryptParams[0]
	enc.KDF.Salt = make([]byte, saltSize)
	if _, err := rand.Read(enc.KDF.Salt); err != nil {
		return nil, err
	}
	enc.Cipher.Name = cipherSecretBox
	enc.Cipher.Nonce = make([]byte, nonceSize)
	if _, err := rand.Read(enc.Cipher.Nonce); err != nil {
		return nil, err
	}

	boxKey, err := scrypt.Key(password, enc.KDF.Salt, enc.KDF.Params.N, enc.KDF.Params.R, enc.KDF.Params.P, boxKeySize)
	if err != nil {
		return nil, err
	}
	var k [boxKeySize]byte
	var n [nonceSize]byte
	copy(k[:], boxKey)
	copy(n[:], enc.Cipher.Nonce)
	enc.Ciphertext = secretbox.Seal(nil, der, &n, &k)

	body, err := json.Marshal(enc)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: PEMTypeEncryptedPrivateKey, Bytes: body}), nil
}

// DecryptPrivateKey decodes an encrypted cosign private key.
func DecryptPrivateKey(pemBytes, password []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found in private key")
	}
	if block.Type != PEMTypeEncryptedPrivateKey && block.Type != PEMTypeEncryptedCosignPrivateKey {
		return nil, fmt.Errorf("unsupported private key PEM type %q", block.Type)
	}

	var enc encryptedKey
	if err := json.Unmarshal(block.Bytes, &enc); err != nil {
		return nil, fmt.Errorf("invalid encrypted private key: %w", err)
	}
	if enc.KDF.Name != kdfScrypt || enc.Cipher.Name != cipherSecretBox {
		return nil, fmt.Errorf("unsupported key encryption %s/%s", enc.KDF.Name, enc.Cipher.Name)
	}
	accepted := false
	for _, p := range acceptedScryptParams {
		if p == enc.KDF.Params {
			accepted = true
			break
		}
	}
	if !accepted {
		return nil, errors.New("unsupported scrypt parameters in private key")
	}
	if len(enc.Cipher.Nonce) != nonceSize {
		return nil, errors.New("invalid nonce in private key")
	}

	boxKey, err := scrypt.Key(password, enc.KDF.Salt, enc.KDF.Params.N, enc.KDF.Params.R, enc.KDF.Params.P, boxKeySize)
	if err != nil {
		return nil, err
	}
	var k [boxKeySize]byte
	var n [nonceSize]byte
	copy(k[:], boxKey)
	copy(n[:], enc.Cipher.Nonce)
	der, ok := secretbox.Open(nil, enc.Ciphertext, &n, &k)
	if !ok {
		return nil, errors.New("decryption of the private key failed: wrong password?")
	}

	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("unsupported private key type %T", key)
	}
	if _, err := hashFor(signer.Public()); err != nil {
		return nil, err
	}
	return signer, nil
}

// LoadPrivateKey reads and decrypts a cosign private key file.
func LoadPrivateKey(path string, password []byte) (crypto.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecryptPrivateKey(b, password)
}

// MarshalPublicKey encodes a public key as PEM.
func MarshalPublicKey(pub crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: PEMTypePublicKey, Bytes: der}), nil
}

// ParsePublicKey decodes a PEM encoded public key.
func ParsePublicKey(pemBytes []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found in public key")
	}
	if block.Type != PEMTypePublicKey {
		return nil, fmt.Errorf("unsupported public key PEM type %q", block.Type)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	if _, err := hashFor(pub); err != nil {
		return nil, err
	}
	return pub, nil
}

// PublicKey is a trusted verification key.
type PublicKey struct {
	Key crypto.PublicKey
	// ID identifies the key, see KeyID.
	ID string
	// Source describes where the key was loaded from, e.g. a file name.
	Source string
}

// LoadPublicKey reads a PEM encoded public key file.
func LoadPublicKey(path string) (*PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pub, err := ParsePublicKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	id, err := KeyID(pub)
	if err != nil {
		return nil, err
	}
	return &PublicKey{Key: pub, ID: id, Source: path}, nil
}

// KeyID returns the identifier of a public key: "sha256:" followed by the hex
// SHA-256 of the key's PKIX DER encoding. It is stable across PEM formatting.
func KeyID(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// hashFor returns the hash used to sign with a key of this type. Like cosign's
// key-based signing, SHA-256 is used for every supported key type; Ed25519
// signs the message itself.
func hashFor(pub crypto.PublicKey) (crypto.Hash, error) {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		return crypto.SHA256, nil
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return 0, errors.New("RSA keys must be at least 2048 bits")
		}
		return crypto.SHA256, nil
	case ed25519.PublicKey:
		return 0, nil
	default:
		return 0, fmt.Errorf("unsupported key type %T", pub)
	}
}
