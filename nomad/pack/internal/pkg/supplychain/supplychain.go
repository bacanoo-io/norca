// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package supplychain connects OCI registries, signatures, trust policies and
// the local pack cache: it downloads packs into the cache after verifying them,
// records where each cached pack came from, and re-verifies cached packs
// before they are rendered or run.
package supplychain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/go-version"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry"

	"github.com/hashicorp/nomad/pack/internal/pkg/caching"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
)

// originVersion is the format version of origin records.
const originVersion = 1

// Origin records where a cached pack was downloaded from. It is stored as
// packlock.OriginFileName in the cached pack directory and holds the signed
// manifest and the signatures, so that the pack can be verified again offline
// against the trust policy in force when it is used.
type Origin struct {
	Version int `json:"version"`
	// Registry is the name of the cache registry the pack belongs to.
	Registry string `json:"registry"`
	// Source is the location the pack was obtained from: an oci:// URL or
	// the file name of a bundle.
	Source string `json:"source"`
	// Repository is "host/repository" of the artifact, if known.
	Repository string `json:"repository,omitempty"`
	// Tag is the tag (pack version) the artifact was obtained by.
	Tag string `json:"tag"`
	// ManifestDigest is the digest of Manifest.
	ManifestDigest string `json:"manifest_digest"`
	// Manifest is the raw, signed manifest.
	Manifest []byte `json:"manifest"`
	// Signatures are the signatures found for the manifest.
	Signatures []signing.Signature `json:"signatures,omitempty"`
}

// ReadOrigin reads the origin record of the pack in dir. It returns nil if
// the pack has none, e.g. because it comes from a git registry.
func ReadOrigin(dir string) (*Origin, error) {
	b, err := os.ReadFile(filepath.Join(dir, packlock.OriginFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var o Origin
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("invalid origin record: %w", err)
	}
	if o.Version != originVersion {
		return nil, fmt.Errorf("unsupported origin record version %d", o.Version)
	}
	return &o, nil
}

func writeOrigin(dir string, o *Origin) error {
	o.Version = originVersion
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, packlock.OriginFileName), b, 0o644)
}

// Result is the provenance of a pack used by a command: where it came from,
// its content digest and the outcome of the signature verification. It is
// what a release records about its pack.
type Result struct {
	Registry  string          `json:"registry"`
	Source    string          `json:"source"`
	Digest    string          `json:"digest"`
	Signature *signing.Result `json:"signature,omitempty"`
}

// SignerKeyID returns the ID of the trusted key that signed the pack, or "".
func (r *Result) SignerKeyID() string {
	if r == nil || r.Signature == nil || !r.Signature.Verified {
		return ""
	}
	return r.Signature.KeyID
}

// PolicyError is returned when a pack does not satisfy the trust policy.
type PolicyError struct {
	Digest string
	Reason string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("trust policy rejects %s: %s", e.Digest, e.Reason)
}

// ApplyPolicy verifies the signatures of the manifest digest with the keys of
// the policy. It fails if the policy requires a signature and none of the
// signatures was made by a trusted key.
func ApplyPolicy(policy packconfig.Policy, digest string, sigs []signing.Signature) (*signing.Result, error) {
	keys, err := policy.LoadKeys()
	if err != nil {
		return nil, err
	}
	if policy.RequireSignature && len(keys) == 0 {
		return nil, &PolicyError{Digest: digest, Reason: "a signature is required but no trusted public keys are configured"}
	}
	res := signing.Verify(digest, sigs, keys)
	if policy.RequireSignature && !res.Verified {
		reason := "no signature by a trusted key"
		if len(sigs) == 0 {
			reason = "the pack is not signed"
		}
		return res, &PolicyError{Digest: digest, Reason: reason}
	}
	return res, nil
}

// ResolveTag returns the tag to download for the requested version. An empty
// request or "latest" selects the highest semantic version tag (ignoring
// pre-releases if a release exists), or the tag "latest" if no tag is a
// semantic version.
func ResolveTag(ctx context.Context, lister registry.TagLister, requested string) (string, error) {
	if requested != "" && requested != caching.DefaultRef {
		return requested, nil
	}
	var tags []string
	err := lister.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to list versions: %w", err)
	}
	return NewestTag(tags)
}

// NewestTag returns the newest version tag, see ResolveTag.
func NewestTag(tags []string) (string, error) {
	var releases, pre []*version.Version
	byVersion := map[*version.Version]string{}
	hasLatest := false
	for _, t := range tags {
		if strings.HasSuffix(t, ".sig") {
			continue
		}
		if t == caching.DefaultRef {
			hasLatest = true
			continue
		}
		v, err := version.NewSemver(t)
		if err != nil {
			continue
		}
		byVersion[v] = t
		if v.Prerelease() == "" {
			releases = append(releases, v)
		} else {
			pre = append(pre, v)
		}
	}
	candidates := releases
	if len(candidates) == 0 {
		candidates = pre
	}
	if len(candidates) > 0 {
		sort.Sort(version.Collection(candidates))
		return byVersion[candidates[len(candidates)-1]], nil
	}
	if hasLatest {
		return caching.DefaultRef, nil
	}
	return "", errors.New("no versions found")
}

// Source is an artifact source: an OCI registry repository or a repository of
// an OCI image layout, e.g. in a bundle.
type Source interface {
	registry.TagLister
	oras.ReadOnlyTarget
}

// FetchOptions describe a download into the cache.
type FetchOptions struct {
	// CacheRoot is the root of the pack cache.
	CacheRoot string
	// Registry is the cache registry name the pack is stored under.
	Registry string
	// RegistrySource is the URL recorded for the cache registry.
	RegistrySource string
	// Location is the location of the artifact repository, for the origin
	// record and the payload identity.
	Location string
	// Repository is "host/repository" of the artifact, if known.
	Repository string
	// Policy is the trust policy to verify the artifact with.
	Policy packconfig.Policy
}

// Fetch downloads the pack packName with the given tag from src, verifies its
// signatures and content and installs it into the cache. It returns the cache
// directory of the pack and the verification result.
func Fetch(ctx context.Context, src Source, packName, tag string, opts FetchOptions) (string, *Result, error) {
	// The names become cache paths: check them before anything else.
	if err := ValidatePathName("cache registry name", opts.Registry); err != nil {
		return "", nil, err
	}
	if err := ValidatePathName("pack name", packName); err != nil {
		return "", nil, err
	}
	if err := ValidateTag(tag); err != nil {
		return "", nil, err
	}
	a, err := oci.Fetch(ctx, src, tag)
	if err != nil {
		return "", nil, err
	}
	if a.Config.Name != packName {
		return "", nil, fmt.Errorf("artifact %s:%s contains pack %q, expected %q", opts.Location, tag, a.Config.Name, packName)
	}
	sigs, err := oci.FetchSignatures(ctx, src, a.Digest())
	if err != nil {
		return "", nil, err
	}
	sigRes, err := ApplyPolicy(opts.Policy, a.Digest(), sigs)
	if err != nil {
		return "", nil, err
	}

	origin := &Origin{
		Registry:       opts.Registry,
		Source:         opts.Location + ":" + tag,
		Repository:     opts.Repository,
		Tag:            tag,
		ManifestDigest: a.Digest(),
		Manifest:       a.Manifest,
		Signatures:     sigs,
	}
	dir, err := install(a, origin, opts)
	if err != nil {
		return "", nil, err
	}
	return dir, &Result{Registry: opts.Registry, Source: origin.Source, Digest: a.Digest(), Signature: sigRes}, nil
}

// PackDir returns the cache directory of a pack version.
func PackDir(cacheRoot, registryName, packName, tag string) string {
	ref := caching.EscapeRef(tag)
	return path.Join(cacheRoot, registryName, ref, caching.AppendRef(caching.EscapePackName(packName), ref))
}

func install(a *oci.Artifact, origin *Origin, opts FetchOptions) (string, error) {
	dest := PackDir(opts.CacheRoot, opts.Registry, a.Config.Name, origin.Tag)
	refDir := path.Dir(dest)
	// Fetch validated the names; never remove or write outside of the
	// cache registry's directory whatever they are.
	registryDir := filepath.Join(opts.CacheRoot, opts.Registry)
	if !within(registryDir, dest) || filepath.Clean(refDir) == filepath.Clean(registryDir) ||
		filepath.Clean(registryDir) == filepath.Clean(opts.CacheRoot) {
		return "", fmt.Errorf("pack %s:%s would be installed outside of the pack cache", a.Config.Name, origin.Tag)
	}
	if err := os.MkdirAll(refDir, caching.DefaultDirPerms); err != nil {
		return "", err
	}
	// Unpack next to the destination and move it into place, so that an
	// interrupted download never leaves a partial pack in the cache. The
	// temporary name has no "@", so the cache never treats it as a pack.
	tmp, err := os.MkdirTemp(refDir, ".download-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	unpacked := filepath.Join(tmp, "pack")
	if err := a.Extract(unpacked); err != nil {
		return "", err
	}
	if err := writeOrigin(unpacked, origin); err != nil {
		return "", err
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.Rename(unpacked, dest); err != nil {
		return "", err
	}

	meta := &caching.Registry{
		Name:     opts.Registry,
		Source:   opts.RegistrySource,
		Ref:      origin.Tag,
		LocalRef: origin.Tag,
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path.Join(refDir, "metadata.json"), b, 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

// VerifyCached verifies a cached pack against its origin record and the
// trust policy, without network access: the recorded manifest must have the
// recorded digest, the pack content must match the tree digest in the
// manifest, and the signatures must satisfy the policy. It returns nil if the
// pack has no origin record.
func VerifyCached(dir string, policy packconfig.Policy) (*Result, error) {
	o, err := ReadOrigin(dir)
	if err != nil || o == nil {
		return nil, err
	}
	if got := oci.ManifestDigest(o.Manifest); got != o.ManifestDigest {
		return nil, fmt.Errorf("origin record of %s is corrupt: manifest digest %s, recorded %s", dir, got, o.ManifestDigest)
	}
	want, err := oci.ParseManifest(o.Manifest)
	if err != nil {
		return nil, err
	}
	got, err := packlock.TreeDigest(dir, packlock.ContentFilter)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("cached pack %s was modified after download (content digest %s, signed %s); download it again", dir, got, want)
	}
	sigRes, err := ApplyPolicy(policy, o.ManifestDigest, o.Signatures)
	if err != nil {
		return nil, err
	}
	return &Result{Registry: o.Registry, Source: o.Source, Digest: o.ManifestDigest, Signature: sigRes}, nil
}
