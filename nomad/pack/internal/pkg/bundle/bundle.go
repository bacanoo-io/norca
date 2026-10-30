// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package bundle implements offline pack bundles, which move packs, their
// dependencies and their signatures into environments without network access
// to the original registries.
//
// A bundle is an uncompressed tar archive (the pack content in it is already
// compressed) with this layout:
//
//	norca-bundle.json                 the bundle index (see Index)
//	repositories/<pack>/oci-layout    one OCI image layout per pack
//	repositories/<pack>/index.json    repository, tagged like the registry
//	repositories/<pack>/blobs/...     repository: "<version>" for the pack
//	                                  and "sha256-<hex>.sig" for signatures
//
// Every repository is a standard OCI image layout, so its content can also be
// handled with generic OCI tools. Bundles are reproducible: the same packs
// produce a byte-identical archive.
package bundle

import (
	"archive/tar"
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	ocistore "oras.land/oras-go/v2/content/oci"

	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
	"github.com/hashicorp/nomad/pack/internal/pkg/supplychain"
)

const (
	// IndexFileName is the name of the bundle index in the archive.
	IndexFileName = "norca-bundle.json"

	// FormatVersion is the bundle format version written by this client.
	FormatVersion = 1

	reposDir = "repositories"

	// maxBundleSize limits the unpacked size of a bundle.
	maxBundleSize = 8 << 30
)

// Index describes the content of a bundle.
type Index struct {
	FormatVersion int      `json:"format_version"`
	Packs         []*Entry `json:"packs"`
}

// Entry is one pack version in a bundle.
type Entry struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Tag is the tag of the pack in its bundle repository.
	Tag string `json:"tag"`
	// Digest is the manifest digest of the pack artifact.
	Digest string `json:"digest"`
	// TreeDigest is the content digest of the pack.
	TreeDigest string `json:"tree_digest"`
	// Source is where the pack was taken from.
	Source string `json:"source"`
	// Signed reports whether signatures are included.
	Signed bool `json:"signed"`
	// DependencyOf lists the packs of the bundle that depend on this one.
	DependencyOf []string `json:"dependency_of,omitempty"`
}

func (idx *Index) find(name, tag string) *Entry {
	for _, e := range idx.Packs {
		if e.Name == name && e.Tag == tag {
			return e
		}
	}
	return nil
}

// Builder assembles a bundle in a temporary directory.
type Builder struct {
	dir    string
	index  *Index
	stores map[string]*ocistore.Store
}

// NewBuilder returns an empty bundle builder. Call Close to remove its
// temporary files.
func NewBuilder() (*Builder, error) {
	dir, err := os.MkdirTemp("", "norca-bundle-")
	if err != nil {
		return nil, err
	}
	return &Builder{dir: dir, index: &Index{FormatVersion: FormatVersion}, stores: map[string]*ocistore.Store{}}, nil
}

// Close removes the builder's temporary files.
func (b *Builder) Close() error {
	return os.RemoveAll(b.dir)
}

// Index returns the index of the packs added so far.
func (b *Builder) Index() *Index {
	return b.index
}

// validName checks a pack name of a bundle, which becomes a path in the
// bundle and in the pack cache.
func validName(name string) error {
	return supplychain.ValidatePathName("pack name", name)
}

func (b *Builder) store(name string) (*ocistore.Store, error) {
	if s, ok := b.stores[name]; ok {
		return s, nil
	}
	if err := validName(name); err != nil {
		return nil, err
	}
	s, err := ocistore.New(filepath.Join(b.dir, reposDir, name))
	if err != nil {
		return nil, err
	}
	b.stores[name] = s
	return s, nil
}

// Add copies the pack with the given tag from src, together with its
// signatures, into the bundle. Adding the same pack version twice is a no-op;
// the returned entry is nil and added is false in that case.
func (b *Builder) Add(ctx context.Context, src oras.ReadOnlyTarget, tag, source string) (*Entry, *oci.Artifact, bool, error) {
	if err := supplychain.ValidateTag(tag); err != nil {
		return nil, nil, false, err
	}
	a, err := oci.Fetch(ctx, src, tag)
	if err != nil {
		return nil, nil, false, err
	}
	if e := b.index.find(a.Config.Name, tag); e != nil {
		if e.Digest != a.Digest() {
			return nil, nil, false, fmt.Errorf("pack %s:%s is included twice with different content (%s, %s)", a.Config.Name, tag, e.Digest, a.Digest())
		}
		return e, a, false, nil
	}
	dst, err := b.store(a.Config.Name)
	if err != nil {
		return nil, nil, false, err
	}
	if err := a.Push(ctx, dst, tag); err != nil {
		return nil, nil, false, err
	}
	signed, err := oci.CopySignatures(ctx, src, dst, a.Digest())
	if err != nil {
		return nil, nil, false, err
	}
	e := &Entry{
		Name:       a.Config.Name,
		Version:    a.Config.Version,
		Tag:        tag,
		Digest:     a.Digest(),
		TreeDigest: a.TreeDigest(),
		Source:     source,
		Signed:     signed,
	}
	b.index.Packs = append(b.index.Packs, e)
	return e, a, true, nil
}

// AddArtifact adds a pack built from a local directory, optionally with a
// signature made by the given key.
func (b *Builder) AddArtifact(ctx context.Context, a *oci.Artifact, tag, source string, key crypto.Signer) (*Entry, error) {
	if err := supplychain.ValidateTag(tag); err != nil {
		return nil, err
	}
	if b.index.find(a.Config.Name, tag) != nil {
		return nil, fmt.Errorf("pack %s:%s is included twice", a.Config.Name, tag)
	}
	dst, err := b.store(a.Config.Name)
	if err != nil {
		return nil, err
	}
	if err := a.Push(ctx, dst, tag); err != nil {
		return nil, err
	}
	signed := false
	if key != nil {
		payload, err := signing.NewPayload(a.Config.Name, a.Digest())
		if err != nil {
			return nil, err
		}
		sig, err := signing.Sign(key, payload)
		if err != nil {
			return nil, err
		}
		if err := oci.AttachSignature(ctx, dst, a.Digest(), signing.Signature{Payload: payload, Signature: sig}, key.Public()); err != nil {
			return nil, err
		}
		signed = true
	}
	e := &Entry{
		Name:       a.Config.Name,
		Version:    a.Config.Version,
		Tag:        tag,
		Digest:     a.Digest(),
		TreeDigest: a.TreeDigest(),
		Source:     source,
		Signed:     signed,
	}
	b.index.Packs = append(b.index.Packs, e)
	return e, nil
}

// Opener returns the artifact source of an OCI location.
type Opener func(loc oci.Location) (supplychain.Source, error)

// AddDependencies adds the dependencies of a with oci:// sources, and
// recursively theirs. Dependencies from other sources must be vendored into
// the pack; their names are returned so the caller can check that.
func (b *Builder) AddDependencies(ctx context.Context, a *oci.Artifact, open Opener) ([]oci.DependencyRef, error) {
	var other []oci.DependencyRef
	for _, d := range a.Config.Dependencies {
		if !oci.IsOCI(d.Source) {
			other = append(other, d)
			continue
		}
		loc, err := oci.ParseLocation(d.Source)
		if err != nil {
			return nil, fmt.Errorf("dependency %q: %w", d.Name, err)
		}
		src, err := open(loc)
		if err != nil {
			return nil, err
		}
		tag := loc.Reference
		if tag == "" {
			if tag, err = supplychain.ResolveTag(ctx, src, d.Ref); err != nil {
				return nil, fmt.Errorf("dependency %q: %w", d.Name, err)
			}
		}
		e, depArtifact, added, err := b.Add(ctx, src, tag, loc.WithReference(tag).String())
		if err != nil {
			return nil, fmt.Errorf("dependency %q: %w", d.Name, err)
		}
		e.DependencyOf = appendUnique(e.DependencyOf, a.Config.Name)
		if added {
			if _, err := b.AddDependencies(ctx, depArtifact, open); err != nil {
				return nil, err
			}
		}
	}
	return other, nil
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// Write writes the bundle archive to w.
func (b *Builder) Write(w io.Writer) error {
	if len(b.index.Packs) == 0 {
		return errors.New("the bundle contains no packs")
	}
	idx, err := json.MarshalIndent(b.index, "", "  ")
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	if err := writeEntry(tw, IndexFileName, int64(len(idx)), strings.NewReader(string(idx))); err != nil {
		return err
	}

	var files []string
	err = filepath.WalkDir(filepath.Join(b.dir, reposDir), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Name() == ocispec.ImageIndexFile {
			if err := normalizeIndex(p); err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(b.dir, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, rel := range files {
		f, err := os.Open(filepath.Join(b.dir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil {
			err = writeEntry(tw, rel, info.Size(), f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// normalizeIndex sorts the manifests of an OCI layout index by tag and digest,
// so that the index does not depend on the order packs were added in.
func normalizeIndex(file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var idx ocispec.Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return err
	}
	sort.SliceStable(idx.Manifests, func(i, j int) bool {
		ri := idx.Manifests[i].Annotations[ocispec.AnnotationRefName]
		rj := idx.Manifests[j].Annotations[ocispec.AnnotationRefName]
		if ri != rj {
			return ri < rj
		}
		return idx.Manifests[i].Digest < idx.Manifests[j].Digest
	})
	out, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o644)
}

func writeEntry(tw *tar.Writer, name string, size int64, r io.Reader) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     size,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := io.CopyN(tw, r, size)
	return err
}

// WriteFile writes the bundle archive to a file.
func (b *Builder) WriteFile(name string) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	if err := b.Write(f); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	return f.Close()
}

// Bundle is an opened bundle archive.
type Bundle struct {
	// Path is the file name of the archive.
	Path  string
	Index *Index
	dir   string
}

// ReadIndex reads only the index of a bundle archive.
func ReadIndex(name string) (*Index, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s has no %s, it is not a pack bundle", name, IndexFileName)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid bundle: %w", err)
		}
		if hdr.Name == IndexFileName {
			return decodeIndex(io.LimitReader(tr, 16<<20))
		}
	}
}

func decodeIndex(r io.Reader) (*Index, error) {
	var idx Index
	if err := json.NewDecoder(r).Decode(&idx); err != nil {
		return nil, fmt.Errorf("invalid bundle index: %w", err)
	}
	if idx.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported bundle format version %d", idx.FormatVersion)
	}
	for _, e := range idx.Packs {
		if e == nil {
			return nil, errors.New("invalid bundle index: empty pack entry")
		}
		if err := validName(e.Name); err != nil {
			return nil, err
		}
		// The tag selects the artifact in the bundle's OCI layout, whose
		// references oras does not validate, and becomes a cache path on
		// import.
		if err := supplychain.ValidateTag(e.Tag); err != nil {
			return nil, fmt.Errorf("pack %s: %w", e.Name, err)
		}
	}
	return &idx, nil
}

// Open unpacks a bundle archive into a temporary directory. Call Close to
// remove it.
func Open(name string) (*Bundle, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dir, err := os.MkdirTemp("", "norca-bundle-")
	if err != nil {
		return nil, err
	}
	b := &Bundle{Path: name, dir: dir}
	if err := b.unpack(f); err != nil {
		b.Close()
		return nil, err
	}
	idxFile, err := os.Open(filepath.Join(dir, IndexFileName))
	if err != nil {
		b.Close()
		return nil, fmt.Errorf("%s has no %s, it is not a pack bundle", name, IndexFileName)
	}
	defer idxFile.Close()
	if b.Index, err = decodeIndex(idxFile); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *Bundle) unpack(r io.Reader) error {
	var total int64
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid bundle: %w", err)
		}
		name := hdr.Name
		clean := path.Clean(name)
		if clean != name || path.IsAbs(name) || strings.Contains(name, `\`) ||
			(name != IndexFileName && !strings.HasPrefix(name, reposDir+"/")) || strings.Contains(name, "..") {
			return fmt.Errorf("invalid path %q in bundle", name)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported entry %q in bundle", name)
		}
		total += hdr.Size
		if total > maxBundleSize {
			return errors.New("bundle exceeds the size limit")
		}
		target := filepath.Join(b.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, err = io.CopyN(out, tr, hdr.Size)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
}

// Close removes the unpacked bundle.
func (b *Bundle) Close() error {
	return os.RemoveAll(b.dir)
}

// Repository returns the OCI layout repository of a pack in the bundle.
func (b *Bundle) Repository(name string) (*ocistore.ReadOnlyStore, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	return ocistore.NewFromFS(context.Background(), os.DirFS(filepath.Join(b.dir, reposDir, name)))
}

// Verify checks every pack of the bundle against the bundle index and the
// trust policy, so that an import changes nothing if any pack is rejected.
func (b *Bundle) Verify(ctx context.Context, policy packconfig.Policy) error {
	for _, e := range b.Index.Packs {
		src, err := b.Repository(e.Name)
		if err != nil {
			return err
		}
		if err := supplychain.ValidateTag(e.Tag); err != nil {
			return fmt.Errorf("pack %s: %w", e.Name, err)
		}
		a, err := oci.Fetch(ctx, src, e.Tag)
		if err != nil {
			return fmt.Errorf("pack %s:%s: %w", e.Name, e.Tag, err)
		}
		if a.Digest() != e.Digest || a.Config.Name != e.Name {
			return fmt.Errorf("pack %s:%s does not match the bundle index", e.Name, e.Tag)
		}
		sigs, err := oci.FetchSignatures(ctx, src, a.Digest())
		if err != nil {
			return err
		}
		if _, err := supplychain.ApplyPolicy(policy, a.Digest(), sigs); err != nil {
			return fmt.Errorf("pack %s:%s: %w", e.Name, e.Tag, err)
		}
	}
	return nil
}

// ImportToCache verifies every pack of the bundle with the policy and
// installs it into the pack cache as registry registryName.
func (b *Bundle) ImportToCache(ctx context.Context, cacheRoot, registryName string, policy packconfig.Policy) ([]*supplychain.Result, error) {
	if err := b.Verify(ctx, policy); err != nil {
		return nil, err
	}
	var results []*supplychain.Result
	location := "bundle:" + filepath.Base(b.Path)
	for _, e := range b.Index.Packs {
		repo, err := b.Repository(e.Name)
		if err != nil {
			return results, err
		}
		_, res, err := supplychain.Fetch(ctx, repo, e.Name, e.Tag, supplychain.FetchOptions{
			CacheRoot:      cacheRoot,
			Registry:       registryName,
			RegistrySource: location,
			Location:       location + "/" + e.Name,
			Policy:         policy,
		})
		if err != nil {
			return results, fmt.Errorf("pack %s:%s: %w", e.Name, e.Tag, err)
		}
		if res.Digest != e.Digest {
			return results, fmt.Errorf("pack %s:%s has digest %s, the bundle index records %s", e.Name, e.Tag, res.Digest, e.Digest)
		}
		results = append(results, res)
	}
	return results, nil
}

// Push verifies every pack of the bundle with the policy and pushes it, with
// its signatures, into the repository below base named like the pack.
func (b *Bundle) Push(ctx context.Context, base oci.Location, opts *oci.Options, policy packconfig.Policy) ([]*supplychain.Result, error) {
	if err := b.Verify(ctx, policy); err != nil {
		return nil, err
	}
	var results []*supplychain.Result
	for _, e := range b.Index.Packs {
		src, err := b.Repository(e.Name)
		if err != nil {
			return results, err
		}
		a, err := oci.Fetch(ctx, src, e.Tag)
		if err != nil {
			return results, fmt.Errorf("pack %s:%s: %w", e.Name, e.Tag, err)
		}
		if a.Digest() != e.Digest || a.Config.Name != e.Name {
			return results, fmt.Errorf("pack %s:%s does not match the bundle index", e.Name, e.Tag)
		}
		sigs, err := oci.FetchSignatures(ctx, src, a.Digest())
		if err != nil {
			return results, err
		}
		sigRes, err := supplychain.ApplyPolicy(policy, a.Digest(), sigs)
		if err != nil {
			return results, fmt.Errorf("pack %s:%s: %w", e.Name, e.Tag, err)
		}
		loc := base.Child(e.Name)
		dst, err := opts.Repository(loc)
		if err != nil {
			return results, err
		}
		if err := a.Push(ctx, dst, e.Tag); err != nil {
			return results, fmt.Errorf("failed to push %s: %w", loc.WithReference(e.Tag), err)
		}
		if _, err := oci.CopySignatures(ctx, src, dst, a.Digest()); err != nil {
			return results, err
		}
		results = append(results, &supplychain.Result{
			Source:    loc.WithReference(e.Tag).String(),
			Digest:    a.Digest(),
			Signature: sigRes,
		})
	}
	return results, nil
}
