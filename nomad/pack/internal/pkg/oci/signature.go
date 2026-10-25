// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"

	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
)

// Signatures are stored the way cosign stores key-based signatures: in an
// image manifest tagged "sha256-<hex>.sig" in the repository of the signed
// artifact, with one layer per signature. Each layer is a simple signing
// payload (media type signing.PayloadMediaType) and carries the base64
// signature in its signing.SignatureAnnotation annotation.

const (
	dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	maxSignatures           = 64
	maxPayloadSize          = 64 << 10
)

// SignatureTag returns the tag that holds the signatures of the manifest with
// the given digest.
func SignatureTag(manifestDigest string) (string, error) {
	alg, hex, ok := strings.Cut(manifestDigest, ":")
	if !ok || alg == "" || hex == "" {
		return "", fmt.Errorf("invalid digest %q", manifestDigest)
	}
	return alg + "-" + hex + ".sig", nil
}

// FetchSignatures returns the signatures of the manifest with the given
// digest. It returns no signatures and no error if the artifact is unsigned.
func FetchSignatures(ctx context.Context, target oras.ReadOnlyTarget, manifestDigest string) ([]signing.Signature, error) {
	m, err := fetchSignatureManifest(ctx, target, manifestDigest)
	if err != nil || m == nil {
		return nil, err
	}

	var sigs []signing.Signature
	for i, l := range m.Layers {
		if i >= maxSignatures {
			break
		}
		if l.MediaType != signing.PayloadMediaType || l.Size > maxPayloadSize {
			continue
		}
		sig := l.Annotations[signing.SignatureAnnotation]
		if sig == "" {
			continue
		}
		payload, err := content.FetchAll(ctx, target, l)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch signature payload: %w", err)
		}
		sigs = append(sigs, signing.Signature{Payload: payload, Signature: sig})
	}
	return sigs, nil
}

func fetchSignatureManifest(ctx context.Context, target oras.ReadOnlyTarget, manifestDigest string) (*ocispec.Manifest, error) {
	tag, err := SignatureTag(manifestDigest)
	if err != nil {
		return nil, err
	}
	_, raw, err := oras.FetchBytes(ctx, target, tag, oras.FetchBytesOptions{MaxBytes: maxManifestSize})
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to fetch signatures: %w", err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid signature manifest: %w", err)
	}
	if m.MediaType != "" && !isManifestMediaType(m.MediaType) {
		return nil, fmt.Errorf("unsupported signature manifest type %q", m.MediaType)
	}
	return &m, nil
}

// signatureConfig is the image config of a signature manifest, in the shape
// of the empty image config cosign creates.
type signatureConfig struct {
	Architecture string `json:"architecture"`
	Created      string `json:"created"`
	History      []struct {
		Created string `json:"created"`
	} `json:"history"`
	OS     string `json:"os"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
	Config struct{} `json:"config"`
}

// AttachSignature adds a signature of the manifest with the given digest to
// target. Existing signatures are kept, except those that verify with the
// public key of the new signature, which it replaces.
func AttachSignature(ctx context.Context, target oras.Target, manifestDigest string, sig signing.Signature, pub crypto.PublicKey) error {
	tag, err := SignatureTag(manifestDigest)
	if err != nil {
		return err
	}
	existing, err := fetchSignatureManifest(ctx, target, manifestDigest)
	if err != nil {
		return err
	}

	var layers []ocispec.Descriptor
	if existing != nil {
		for _, l := range existing.Layers {
			if l.MediaType == signing.PayloadMediaType && pub != nil {
				if payload, err := content.FetchAll(ctx, target, l); err == nil &&
					signing.VerifySignature(pub, payload, l.Annotations[signing.SignatureAnnotation]) == nil {
					continue
				}
			}
			layers = append(layers, l)
		}
	}

	layer := content.NewDescriptorFromBytes(signing.PayloadMediaType, sig.Payload)
	layer.Annotations = map[string]string{signing.SignatureAnnotation: sig.Signature}
	if err := pushIfMissing(ctx, target, layer, sig.Payload); err != nil {
		return err
	}
	layers = append(layers, layer)

	var cfg signatureConfig
	cfg.Created = "0001-01-01T00:00:00Z"
	cfg.History = make([]struct {
		Created string `json:"created"`
	}, len(layers))
	for i := range cfg.History {
		cfg.History[i].Created = cfg.Created
	}
	cfg.RootFS.Type = "layers"
	for _, l := range layers {
		// The payloads are uncompressed, so the diff IDs are the digests.
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, l.Digest.String())
	}
	cfgBlob, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	cfgDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageConfig, cfgBlob)
	if err := pushIfMissing(ctx, target, cfgDesc, cfgBlob); err != nil {
		return err
	}

	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfgDesc,
		Layers:    layers,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = oras.TagBytes(ctx, target, ocispec.MediaTypeImageManifest, raw, tag)
	return err
}

// CopySignatures copies the signature manifest of the given digest, if any,
// from src to dst. It reports whether signatures were copied.
func CopySignatures(ctx context.Context, src oras.ReadOnlyTarget, dst oras.Target, manifestDigest string) (bool, error) {
	tag, err := SignatureTag(manifestDigest)
	if err != nil {
		return false, err
	}
	if _, err := src.Resolve(ctx, tag); err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to resolve signatures: %w", err)
	}
	if _, err := oras.Copy(ctx, src, tag, dst, tag, oras.DefaultCopyOptions); err != nil {
		return false, fmt.Errorf("failed to copy signatures: %w", err)
	}
	return true, nil
}

// isManifestMediaType reports whether mt is a manifest type signatures may be
// stored with. cosign versions differ in the type they write.
func isManifestMediaType(mt string) bool {
	return mt == ocispec.MediaTypeImageManifest || mt == dockerManifestMediaType
}
