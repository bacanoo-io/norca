// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package sign

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

// privateKeyType is the PEM type of encrypted private key, the format
// of cosign: PKCS #8 encrypted with nacl/secretbox under a key derived
// with scrypt from a password.
const privateKeyType = "ENCRYPTED SIGSTORE PRIVATE KEY"

// The parameters of scrypt for a new key.
const (
	scryptN = 32768
	scryptR = 8
	scryptP = 1
)

// The bounds of the parameters of scrypt of a key that is read. A key file
// names its own, and scrypt takes 128*N*r bytes and p times the work: a file
// is not let ask for more than a key file needs (this package writes 32 MiB,
// cosign 64 MiB, both with p of 1).
const (
	maxScryptBytes = 256 << 20
	maxScryptP     = 16
)

// checkScrypt refuses parameters of scrypt outside the bounds, before any
// of their memory is taken.
func checkScrypt(n, r, p int) error {
	if n < 2 || r < 1 || p < 1 || p > maxScryptP || r > maxScryptBytes/128 || n > maxScryptBytes/128/r {
		return fmt.Errorf("%w: the parameters of scrypt N=%d r=%d p=%d are out of bounds (at most %d MiB, p %d)",
			ErrKey, n, r, p, maxScryptBytes>>20, maxScryptP)
	}

	return nil
}

// encryptedKey is the content of the PEM block.
type encryptedKey struct {
	KDF struct {
		Name   string `json:"name"`
		Params struct {
			N int `json:"N"`
			R int `json:"r"`
			P int `json:"p"`
		} `json:"params"`
		Salt []byte `json:"salt"`
	} `json:"kdf"`
	Cipher struct {
		Name  string `json:"name"`
		Nonce []byte `json:"nonce"`
	} `json:"cipher"`
	Ciphertext []byte `json:"ciphertext"`
}

// ErrPassword is the error of a password that does not open a key.
var ErrPassword = errors.New("the password does not open the key")

// MarshalPrivateKey writes a private key encrypted with password.
func MarshalPrivateKey(key *ecdsa.PrivateKey, password []byte) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}

	var ek encryptedKey

	ek.KDF.Name = "scrypt"
	ek.KDF.Params.N, ek.KDF.Params.R, ek.KDF.Params.P = scryptN, scryptR, scryptP
	ek.KDF.Salt = make([]byte, 32)
	ek.Cipher.Name = "nacl/secretbox"
	ek.Cipher.Nonce = make([]byte, 24)

	if _, err := rand.Read(ek.KDF.Salt); err != nil {
		return nil, err
	}

	if _, err := rand.Read(ek.Cipher.Nonce); err != nil {
		return nil, err
	}

	secret, err := scrypt.Key(password, ek.KDF.Salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, err
	}

	var k [32]byte

	var nonce [24]byte

	copy(k[:], secret)
	copy(nonce[:], ek.Cipher.Nonce)
	ek.Ciphertext = secretbox.Seal(nil, der, &nonce, &k)

	data, err := json.Marshal(ek)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: privateKeyType, Bytes: data}), nil
}

// ParsePrivateKey reads a private key encrypted with password.
func ParsePrivateKey(text, password []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(text)
	if block == nil || block.Type != privateKeyType {
		return nil, fmt.Errorf("%w: no %s", ErrKey, privateKeyType)
	}

	var ek encryptedKey
	if err := json.Unmarshal(block.Bytes, &ek); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	if ek.KDF.Name != "scrypt" || ek.Cipher.Name != "nacl/secretbox" || len(ek.Cipher.Nonce) != 24 {
		return nil, fmt.Errorf("%w: kdf %s, cipher %s", ErrKey, ek.KDF.Name, ek.Cipher.Name)
	}

	if err := checkScrypt(ek.KDF.Params.N, ek.KDF.Params.R, ek.KDF.Params.P); err != nil {
		return nil, err
	}

	secret, err := scrypt.Key(password, ek.KDF.Salt, ek.KDF.Params.N, ek.KDF.Params.R, ek.KDF.Params.P, 32)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	var k [32]byte

	var nonce [24]byte

	copy(k[:], secret)
	copy(nonce[:], ek.Cipher.Nonce)

	der, ok := secretbox.Open(nil, ek.Ciphertext, &nonce, &k)
	if !ok {
		return nil, ErrPassword
	}

	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}

	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: not an ECDSA key", ErrKey)
	}

	return ec, nil
}
