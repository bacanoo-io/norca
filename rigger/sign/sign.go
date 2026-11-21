// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package sign signs and verifies the tree digest of a module (design 8.1):
// cosign-format ECDSA P-256 keys, and a cosign simple signing payload whose
// docker-manifest-digest is the tree digest. It is the statement the
// admission of Norca verifies for trusted_keys, with the same key IDs: the
// SHA-256 of the PKIX DER encoding of the public key.
package sign

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// SignatureDir is the directory of the signatures of a module, inside the
// module. It is hidden, so the tree digest it signs leaves it out (design
// 8.1): the signatures go with the module from any source.
const SignatureDir = ".rigger-signatures"

// Signature is a signed statement about a tree digest.
type Signature struct {
	// Payload is the cosign simple signing payload.
	Payload string `json:"payload"`
	// Signature is the base64 of the ASN.1 ECDSA signature of the SHA-256
	// of the payload.
	Signature string `json:"signature"`
}

// PublicKey is a key that verifies signatures.
type PublicKey struct {
	Key *ecdsa.PublicKey
	// ID is "sha256:" and the hex SHA-256 of the PKIX DER encoding.
	ID string
}

// ErrKey is the error of a key that can't be read.
var ErrKey = errors.New("invalid key")

// GenerateKey makes an ECDSA P-256 key.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// MarshalPublicKey writes the PEM of a public key, the format of cosign and
// of Norca's trusted_keys.
func MarshalPublicKey(pub *ecdsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// ParsePublicKey reads the PEM of an ECDSA public key.
func ParsePublicKey(text []byte) (PublicKey, error) {
	block, _ := pem.Decode(text)
	if block == nil || block.Type != "PUBLIC KEY" {
		return PublicKey{}, fmt.Errorf("%w: no PEM public key", ErrKey)
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: %w", ErrKey, err)
	}

	key, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return PublicKey{}, fmt.Errorf("%w: not an ECDSA key", ErrKey)
	}

	sum := sha256.Sum256(block.Bytes)

	return PublicKey{Key: key, ID: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

// KeyID is the ID of a public key.
func KeyID(pub *ecdsa.PublicKey) (string, error) {
	text, err := MarshalPublicKey(pub)
	if err != nil {
		return "", err
	}

	k, err := ParsePublicKey(text)

	return k.ID, err
}

// statement is the cosign simple signing payload.
type statement struct {
	Critical struct {
		Identity struct {
			DockerReference string `json:"docker-reference"`
		} `json:"identity"`
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
	Optional map[string]string `json:"optional,omitempty"`
}

// Statement is the payload Sign signs for the tree digest of the version
// of a module. A registry that keeps only the signature (the global
// registry, design 7.3) builds the payload again with it.
func Statement(digest, module, version string) (string, error) {
	var st statement

	st.Critical.Identity.DockerReference = "rigger/" + module
	st.Critical.Image.DockerManifestDigest = digest
	st.Critical.Type = "cosign container image signature"
	st.Optional = map[string]string{"rigger.module": module, "rigger.version": version}

	payload, err := json.Marshal(st)
	if err != nil {
		return "", err
	}

	return string(payload), nil
}

// Sign signs the tree digest of the version of a module.
func Sign(key *ecdsa.PrivateKey, digest, module, version string) (Signature, error) {
	text, err := Statement(digest, module, version)
	if err != nil {
		return Signature{}, err
	}

	payload := []byte(text)
	sum := sha256.Sum256(payload)

	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return Signature{}, err
	}

	return Signature{Payload: string(payload), Signature: base64.StdEncoding.EncodeToString(sig)}, nil
}

// Verify returns the IDs of the keys of which a signature signs the
// digest, sorted, without repetition.
func Verify(digest string, sigs []Signature, keys []PublicKey) []string {
	var signers []string

	for _, s := range sigs {
		var st statement
		if err := json.Unmarshal([]byte(s.Payload), &st); err != nil || st.Critical.Image.DockerManifestDigest != digest {
			continue
		}

		raw, err := base64.StdEncoding.DecodeString(s.Signature)
		if err != nil {
			continue
		}

		sum := sha256.Sum256([]byte(s.Payload))

		for _, k := range keys {
			if ecdsa.VerifyASN1(k.Key, sum[:], raw) && !slices.Contains(signers, k.ID) {
				signers = append(signers, k.ID)
			}
		}
	}

	slices.Sort(signers)

	return signers
}

// FileName is the name of the signature file of a key in SignatureDir.
func FileName(keyID string) string {
	return strings.TrimPrefix(keyID, "sha256:") + ".json"
}

// ReadSignatures reads the signatures in SignatureDir of the module in
// directory dir of fsys; none when there is no such directory.
func ReadSignatures(fsys fs.FS, dir string) ([]Signature, error) {
	sigDir := path.Join(dir, SignatureDir)

	entries, err := fs.ReadDir(fsys, sigDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var sigs []Signature

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		data, err := fs.ReadFile(fsys, path.Join(sigDir, e.Name()))
		if err != nil {
			return nil, err
		}

		var s Signature
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}

		sigs = append(sigs, s)
	}

	return sigs, nil
}
