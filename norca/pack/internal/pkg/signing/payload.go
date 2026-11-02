// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package signing

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// PayloadType is the "critical.type" of cosign's simple signing payload.
	PayloadType = "cosign container image signature"

	// PayloadMediaType is the media type of the signature layers that hold
	// the simple signing payload.
	PayloadMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"

	// SignatureAnnotation is the layer annotation that holds the base64
	// encoded signature of the payload.
	SignatureAnnotation = "dev.cosignproject.cosign/signature"
)

// Payload is cosign's "simple signing" payload: a statement that the manifest
// with the given digest, published at the given repository, is signed. The
// field order matches cosign's JSON encoding.
type Payload struct {
	Critical Critical       `json:"critical"`
	Optional map[string]any `json:"optional"`
}

// Critical holds the signed claims of a payload.
type Critical struct {
	Identity Identity `json:"identity"`
	Image    Image    `json:"image"`
	Type     string   `json:"type"`
}

// Identity names the repository the signed artifact was published at.
type Identity struct {
	DockerReference string `json:"docker-reference"`
}

// Image holds the digest of the signed manifest.
type Image struct {
	DockerManifestDigest string `json:"docker-manifest-digest"`
}

// NewPayload returns the payload signing the manifest digest published in the
// repository (registry host and repository path, without tag or digest).
func NewPayload(repository, manifestDigest string) ([]byte, error) {
	return json.Marshal(Payload{
		Critical: Critical{
			Identity: Identity{DockerReference: repository},
			Image:    Image{DockerManifestDigest: manifestDigest},
			Type:     PayloadType,
		},
	})
}

// ParsePayload decodes and validates a simple signing payload.
func ParsePayload(b []byte) (*Payload, error) {
	var p Payload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("invalid signature payload: %w", err)
	}
	if p.Critical.Type != PayloadType {
		return nil, fmt.Errorf("unsupported signature payload type %q", p.Critical.Type)
	}
	if p.Critical.Image.DockerManifestDigest == "" {
		return nil, errors.New("signature payload has no manifest digest")
	}
	return &p, nil
}

// Sign signs payload with key and returns the base64 encoded signature, the
// value of the SignatureAnnotation.
func Sign(key crypto.Signer, payload []byte) (string, error) {
	h, err := hashFor(key.Public())
	if err != nil {
		return "", err
	}
	digest := payload
	var opts crypto.SignerOpts = crypto.Hash(0)
	if h != 0 {
		hasher := h.New()
		hasher.Write(payload)
		digest = hasher.Sum(nil)
		opts = h
	}
	sig, err := key.Sign(rand.Reader, digest, opts)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifySignature checks the base64 encoded signature of payload with pub.
func VerifySignature(pub crypto.PublicKey, payload []byte, signatureB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}
	h, err := hashFor(pub)
	if err != nil {
		return err
	}
	var digest []byte
	if h != 0 {
		hasher := h.New()
		hasher.Write(payload)
		digest = hasher.Sum(nil)
	}

	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(k, digest, sig) {
			return errors.New("invalid signature")
		}
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(k, h, digest, sig); err != nil {
			return errors.New("invalid signature")
		}
	case ed25519.PublicKey:
		if !ed25519.Verify(k, payload, sig) {
			return errors.New("invalid signature")
		}
	default:
		return fmt.Errorf("unsupported key type %T", pub)
	}
	return nil
}

// Signature is one signature of an artifact.
type Signature struct {
	// Payload is the signed simple signing payload.
	Payload []byte `json:"payload"`
	// Signature is the base64 encoded signature.
	Signature string `json:"signature"`
}

// Result is the outcome of verifying the signatures of an artifact.
type Result struct {
	// Digest is the manifest digest that was verified.
	Digest string `json:"digest"`
	// Verified is true if a signature by a trusted key was found.
	Verified bool `json:"verified"`
	// KeyID identifies the trusted key that signed the digest.
	KeyID string `json:"key_id,omitempty"`
	// KeySource is the file the trusted key was loaded from.
	KeySource string `json:"key_source,omitempty"`
}

// Verify returns the result of checking the signatures of the manifest with
// the given digest against the trusted keys. A signature counts if it is valid
// for one of the keys and its payload claims exactly this digest. The
// repository named in the payload is not compared, so that artifacts copied
// to a mirror or an internal registry keep their signatures.
func Verify(manifestDigest string, sigs []Signature, keys []*PublicKey) *Result {
	res := &Result{Digest: manifestDigest}
	for _, s := range sigs {
		p, err := ParsePayload(s.Payload)
		if err != nil || p.Critical.Image.DockerManifestDigest != manifestDigest {
			continue
		}
		for _, k := range keys {
			if VerifySignature(k.Key, s.Payload, s.Signature) == nil {
				res.Verified = true
				res.KeyID = k.ID
				res.KeySource = k.Source
				return res
			}
		}
	}
	return res
}
