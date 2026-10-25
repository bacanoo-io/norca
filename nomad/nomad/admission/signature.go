// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
)

// Verification of cosign compatible pack signatures for the trusted keys of
// release admission policies. A signature is a "simple signing" payload
// naming the manifest digest, and the base64 signature of the payload's
// SHA-256 (Ed25519 signs the payload itself), as written by norca pack sign
// and cosign sign --key.

// PackSignature is a signature of a pack.
type PackSignature struct {
	Payload   []byte
	Signature string
}

// TrustedKey is a parsed trusted public key.
type TrustedKey struct {
	Key crypto.PublicKey
	ID  string
}

// ParseTrustedKey parses a PEM encoded public key and computes its ID:
// "sha256:" followed by the hex SHA-256 of its PKIX DER encoding, the key ID
// norca pack prints.
func ParseTrustedKey(pemText string) (*TrustedKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("no PEM encoded public key found")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid public key: %w", err)
	}
	switch k := pub.(type) {
	case *ecdsa.PublicKey, ed25519.PublicKey:
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, errors.New("RSA keys must be at least 2048 bits")
		}
	default:
		return nil, fmt.Errorf("unsupported key type %T", pub)
	}
	sum := sha256.Sum256(block.Bytes)
	return &TrustedKey{Key: pub, ID: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

// simpleSigningPayload is the part of the payload that is checked.
type simpleSigningPayload struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

// VerifyPackSignatures returns the ID of the first trusted key that made one
// of the signatures over a payload naming digest, or "" if none did.
func VerifyPackSignatures(digest string, sigs []*PackSignature, keys []*TrustedKey) string {
	if digest == "" {
		return ""
	}
	for _, s := range sigs {
		if s == nil {
			continue
		}
		var p simpleSigningPayload
		if err := json.Unmarshal(s.Payload, &p); err != nil || p.Critical.Image.DockerManifestDigest != digest {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(s.Signature)
		if err != nil {
			continue
		}
		for _, k := range keys {
			if verifySignature(k.Key, s.Payload, raw) {
				return k.ID
			}
		}
	}
	return ""
}

func verifySignature(pub crypto.PublicKey, payload, sig []byte) bool {
	sum := sha256.Sum256(payload)
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(k, sum[:], sig)
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) == nil
	case ed25519.PublicKey:
		return ed25519.Verify(k, payload, sig)
	}
	return false
}
