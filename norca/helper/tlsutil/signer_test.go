// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tlsutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"io"
	"testing"
)

// opaqueSigner is a key that signs elsewhere, as a key of the TPM does.
type opaqueSigner struct{ pub crypto.PublicKey }

func (s opaqueSigner) Public() crypto.PublicKey { return s.pub }
func (s opaqueSigner) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, nil
}

// The cipher suites are checked against the key of a crypto.Signer by its
// public key.
func TestGetSignatureAlgorithm_Signer(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	for name, tc := range map[string]struct {
		key  crypto.PrivateKey
		want signatureAlgorithm
	}{
		"ecdsa signer": {opaqueSigner{ec.Public()}, ecdsaStringRepr},
		"rsa signer":   {opaqueSigner{rk.Public()}, rsaStringRepr},
		"ecdsa":        {ec, ecdsaStringRepr},
	} {
		got, err := getSignatureAlgorithm(&tls.Certificate{PrivateKey: tc.key})
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, tc.want)
		}
	}
	if _, err := getSignatureAlgorithm(&tls.Certificate{PrivateKey: opaqueSigner{ed.Public()}}); err == nil {
		t.Error("an ed25519 signer was accepted")
	}
}
