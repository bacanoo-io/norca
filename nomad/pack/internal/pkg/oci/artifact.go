// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"

	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/sdk/pack"
)

const (
	// ArtifactType identifies a Norca pack artifact.
	ArtifactType = "application/vnd.norca.pack.v1"

	// ConfigMediaType is the media type of the artifact config, a JSON
	// summary of the pack's metadata (see PackConfig).
	ConfigMediaType = "application/vnd.norca.pack.config.v1+json"

	// ContentMediaType is the media type of the single layer that holds the
	// pack directory as a gzip compressed tar archive.
	ContentMediaType = "application/vnd.norca.pack.content.v1.tar+gzip"

	// AnnotationTreeDigest is the manifest annotation with the tree digest
	// (see packlock.TreeDigest) of the full pack content. As part of the
	// signed manifest it lets clients verify an unpacked pack offline.
	AnnotationTreeDigest = "dev.norca.pack.tree-digest"

	// AnnotationPackName is the manifest annotation with the pack name.
	AnnotationPackName = "dev.norca.pack.name"

	// MaxContentSize limits the size of a pack's compressed content.
	MaxContentSize = 64 << 20
	// maxUnpackedSize limits the size of a pack's unpacked content.
	maxUnpackedSize = 256 << 20
	// maxManifestSize limits the size of manifests and configs.
	maxManifestSize = 4 << 20
)

// Target is a store artifacts can be pushed to: a registry repository or an
// OCI image layout.
type Target = oras.Target

// PackConfig is the content of the artifact config blob.
type PackConfig struct {
	Name         string          `json:"name"`
	Version      string          `json:"version,omitempty"`
	Description  string          `json:"description,omitempty"`
	URL          string          `json:"url,omitempty"`
	Dependencies []DependencyRef `json:"dependencies,omitempty"`
	TreeDigest   string          `json:"tree_digest"`
}

// DependencyRef is a dependency declared in the pack's metadata.
type DependencyRef struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// Artifact is a pack in its OCI form.
type Artifact struct {
	// Descriptor describes the manifest.
	Descriptor ocispec.Descriptor
	// Manifest is the raw manifest.
	Manifest []byte
	// Config is the decoded config blob.
	Config PackConfig

	configBlob []byte
	content    []byte
	parsed     ocispec.Manifest
}

// Digest returns the manifest digest of the artifact.
func (a *Artifact) Digest() string {
	return a.Descriptor.Digest.String()
}

// TreeDigest returns the tree digest recorded in the signed manifest.
func (a *Artifact) TreeDigest() string {
	return a.parsed.Annotations[AnnotationTreeDigest]
}

// Build creates the artifact of the pack in dir. The artifact is
// reproducible: building unchanged content yields the same manifest digest.
// If the environment sets SOURCE_DATE_EPOCH, it is recorded as the creation
// time of the manifest.
func Build(dir string, metadata *pack.Metadata) (*Artifact, error) {
	if metadata == nil || metadata.Pack == nil || metadata.Pack.Name == "" {
		return nil, errors.New("the pack metadata must define a pack name")
	}
	treeDigest, err := packlock.TreeDigest(dir, packlock.ContentFilter)
	if err != nil {
		return nil, err
	}
	layer, err := archive(dir)
	if err != nil {
		return nil, err
	}
	if len(layer) > MaxContentSize {
		return nil, fmt.Errorf("pack content is %d bytes, more than the limit of %d", len(layer), MaxContentSize)
	}

	cfg := PackConfig{
		Name:        metadata.Pack.Name,
		Version:     metadata.Pack.Version,
		Description: metadata.Pack.Description,
		URL:         metadata.Pack.URL,
		TreeDigest:  treeDigest,
	}
	for _, d := range metadata.Dependencies {
		if d.Enabled != nil && !*d.Enabled {
			continue
		}
		cfg.Dependencies = append(cfg.Dependencies, DependencyRef{Name: d.Name, Source: d.Source, Ref: d.Ref})
	}
	configBlob, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}

	annotations := map[string]string{
		ocispec.AnnotationTitle: metadata.Pack.Name,
		AnnotationPackName:      metadata.Pack.Name,
		AnnotationTreeDigest:    treeDigest,
	}
	if metadata.Pack.Version != "" {
		annotations[ocispec.AnnotationVersion] = metadata.Pack.Version
	}
	if metadata.Pack.Description != "" {
		annotations[ocispec.AnnotationDescription] = metadata.Pack.Description
	}
	if metadata.Pack.URL != "" {
		annotations[ocispec.AnnotationURL] = metadata.Pack.URL
	}
	if epoch := os.Getenv("SOURCE_DATE_EPOCH"); epoch != "" {
		sec, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid SOURCE_DATE_EPOCH: %w", err)
		}
		annotations[ocispec.AnnotationCreated] = time.Unix(sec, 0).UTC().Format(time.RFC3339)
	}

	layerName := metadata.Pack.Name
	if metadata.Pack.Version != "" {
		layerName += "-" + metadata.Pack.Version
	}
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: ArtifactType,
		Config:       content.NewDescriptorFromBytes(ConfigMediaType, configBlob),
		Layers: []ocispec.Descriptor{
			withAnnotations(content.NewDescriptorFromBytes(ContentMediaType, layer),
				map[string]string{ocispec.AnnotationTitle: layerName + ".tar.gz"}),
		},
		Annotations: annotations,
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifest)
	desc.ArtifactType = ArtifactType

	return &Artifact{
		Descriptor: desc,
		Manifest:   manifest,
		Config:     cfg,
		configBlob: configBlob,
		content:    layer,
		parsed:     m,
	}, nil
}

func withAnnotations(d ocispec.Descriptor, a map[string]string) ocispec.Descriptor {
	d.Annotations = a
	return d
}

// Push uploads the artifact into target and tags it. The tag may be empty to
// push the artifact by digest only.
func (a *Artifact) Push(ctx context.Context, target oras.Target, tag string) error {
	blobs := []struct {
		desc ocispec.Descriptor
		data []byte
	}{
		{a.parsed.Config, a.configBlob},
		{a.parsed.Layers[0], a.content},
	}
	for _, b := range blobs {
		if err := pushIfMissing(ctx, target, b.desc, b.data); err != nil {
			return err
		}
	}
	if tag == "" {
		return pushIfMissing(ctx, target, a.Descriptor, a.Manifest)
	}
	_, err := oras.TagBytes(ctx, target, a.Descriptor.MediaType, a.Manifest, tag)
	return err
}

func pushIfMissing(ctx context.Context, target content.Storage, desc ocispec.Descriptor, data []byte) error {
	exists, err := target.Exists(ctx, desc)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	err = target.Push(ctx, desc, bytes.NewReader(data))
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return nil
}

// Fetch downloads the artifact with the given tag or digest from target and
// checks its media types and digests.
func Fetch(ctx context.Context, target oras.ReadOnlyTarget, reference string) (*Artifact, error) {
	desc, manifest, err := oras.FetchBytes(ctx, target, reference, oras.FetchBytesOptions{MaxBytes: maxManifestSize})
	if err != nil {
		return nil, err
	}
	return fetchFromManifest(ctx, target, desc, manifest)
}

func fetchFromManifest(ctx context.Context, target content.Fetcher, desc ocispec.Descriptor, manifest []byte) (*Artifact, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	if m.MediaType != ocispec.MediaTypeImageManifest || m.Config.MediaType != ConfigMediaType {
		return nil, fmt.Errorf("%s is not a Norca pack (manifest %q, config %q)", desc.Digest, m.MediaType, m.Config.MediaType)
	}
	if len(m.Layers) != 1 || m.Layers[0].MediaType != ContentMediaType {
		return nil, fmt.Errorf("%s is not a Norca pack: unexpected layers", desc.Digest)
	}
	if m.Config.Size > maxManifestSize || m.Layers[0].Size > MaxContentSize {
		return nil, fmt.Errorf("%s exceeds the size limits of a pack", desc.Digest)
	}

	configBlob, err := content.FetchAll(ctx, target, m.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch pack config: %w", err)
	}
	var cfg PackConfig
	if err := json.Unmarshal(configBlob, &cfg); err != nil {
		return nil, fmt.Errorf("invalid pack config: %w", err)
	}
	layer, err := content.FetchAll(ctx, target, m.Layers[0])
	if err != nil {
		return nil, fmt.Errorf("failed to fetch pack content: %w", err)
	}
	if m.Annotations[AnnotationTreeDigest] == "" {
		return nil, fmt.Errorf("%s has no %s annotation", desc.Digest, AnnotationTreeDigest)
	}
	return &Artifact{
		Descriptor: desc,
		Manifest:   manifest,
		Config:     cfg,
		configBlob: configBlob,
		content:    layer,
		parsed:     m,
	}, nil
}

// ParseManifest decodes a raw pack manifest, e.g. one recorded in an origin
// record, and returns the tree digest recorded in it.
func ParseManifest(manifest []byte) (treeDigest string, err error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return "", fmt.Errorf("invalid manifest: %w", err)
	}
	if m.Config.MediaType != ConfigMediaType {
		return "", errors.New("manifest is not a Norca pack manifest")
	}
	td := m.Annotations[AnnotationTreeDigest]
	if td == "" {
		return "", fmt.Errorf("manifest has no %s annotation", AnnotationTreeDigest)
	}
	return td, nil
}

// ManifestDigest returns the digest of raw manifest bytes.
func ManifestDigest(manifest []byte) string {
	return digest.FromBytes(manifest).String()
}

// Extract unpacks the pack content into dest, which must not exist yet, and
// verifies the unpacked files against the tree digest of the manifest.
func (a *Artifact) Extract(dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	if err := unarchive(a.content, dest); err != nil {
		_ = os.RemoveAll(dest)
		return err
	}
	got, err := packlock.TreeDigest(dest, packlock.ContentFilter)
	if err != nil {
		_ = os.RemoveAll(dest)
		return err
	}
	if want := a.TreeDigest(); got != want {
		_ = os.RemoveAll(dest)
		return fmt.Errorf("unpacked pack content digest %s does not match the manifest (%s)", got, want)
	}
	return nil
}

// archive writes the content of dir into a reproducible gzip compressed tar
// archive: sorted entries, regular files only, no timestamps, owners or
// permissions beyond a fixed mode.
func archive(dir string) ([]byte, error) {
	files, err := packlock.Files(dir, packlock.ContentFilter)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)
	for _, f := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.Path,
			Mode:     0o644,
			Size:     f.Size,
			ModTime:  time.Unix(0, 0),
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if err := copyFile(tw, f.Abs, f.Size); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func copyFile(w io.Writer, name string, size int64) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, f)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s changed while archiving", name)
	}
	return nil
}

// unarchive safely unpacks a pack archive into dest. Only regular files and
// directories with relative paths inside dest are accepted.
func unarchive(data []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid pack archive: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	var total int64
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid pack archive: %w", err)
		}
		name := hdr.Name
		clean := path.Clean(name)
		if name == "" || path.IsAbs(name) || strings.Contains(name, "\\") || clean == ".." ||
			strings.HasPrefix(clean, "../") || filepath.VolumeName(name) != "" {
			return fmt.Errorf("invalid path %q in pack archive", name)
		}
		target := filepath.Join(dest, filepath.FromSlash(clean))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxUnpackedSize {
				return errors.New("pack archive exceeds the size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, err = io.CopyN(f, tr, hdr.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q of type %c in pack archive", name, hdr.Typeflag)
		}
	}
}
