// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bundle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci/ocitest"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
	"github.com/hashicorp/nomad/pack/internal/pkg/supplychain"
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
	"github.com/hashicorp/nomad/pack/sdk/pack"
)

var plain = &oci.Options{PlainHTTP: true, NoDockerConfig: true}

// publish builds the pack fixture under the given name with the given
// dependencies, pushes it and signs it.
func publish(t *testing.T, srv *ocitest.Server, name string, deps []*pack.Dependency, key *signing.KeyPair) *oci.Artifact {
	t.Helper()
	ctx := context.Background()
	dir := testfixture.Clone(t, "v2/test_registry/packs/simple_raw_exec")
	p, err := loader.Load(dir)
	must.NoError(t, err)
	meta := *p.Metadata
	mp := *meta.Pack
	mp.Name = name
	meta.Pack = &mp
	meta.Dependencies = deps
	a, err := oci.Build(dir, &meta)
	must.NoError(t, err)

	loc := oci.Location{Host: srv.Host(), Repository: "packs/" + name}
	repo, err := plain.Repository(loc)
	must.NoError(t, err)
	must.NoError(t, a.Push(ctx, repo, "1.0.0"))

	if key != nil {
		priv, err := signing.DecryptPrivateKey(key.PrivateKeyPEM, nil)
		must.NoError(t, err)
		payload, err := signing.NewPayload(loc.RepositoryName(), a.Digest())
		must.NoError(t, err)
		sig, err := signing.Sign(priv, payload)
		must.NoError(t, err)
		must.NoError(t, oci.AttachSignature(ctx, repo, a.Digest(), signing.Signature{Payload: payload, Signature: sig}, priv.Public()))
	}
	return a
}

func TestBundle_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The internet side: a registry with a signed pack and its dependency.
	src := ocitest.New().Start()
	defer src.Close()
	kp, err := signing.GenerateKeyPair(nil)
	must.NoError(t, err)
	keyFile := filepath.Join(t.TempDir(), "cosign.pub")
	must.NoError(t, os.WriteFile(keyFile, kp.PublicKeyPEM, 0o644))

	child := publish(t, src, "child", nil, kp)
	parent := publish(t, src, "parent", []*pack.Dependency{{
		Name:   "child",
		Source: "oci://" + src.Host() + "/packs/child",
		Ref:    "1.0.0",
	}}, kp)

	open := func(loc oci.Location) (supplychain.Source, error) { return plain.Repository(loc) }

	build := func() []byte {
		b, err := NewBuilder()
		must.NoError(t, err)
		defer b.Close()
		repo, err := plain.Repository(oci.Location{Host: src.Host(), Repository: "packs/parent"})
		must.NoError(t, err)
		e, a, added, err := b.Add(ctx, repo, "1.0.0", "oci://"+src.Host()+"/packs/parent:1.0.0")
		must.NoError(t, err)
		must.True(t, added)
		must.True(t, e.Signed)
		other, err := b.AddDependencies(ctx, a, open)
		must.NoError(t, err)
		must.SliceEmpty(t, other)
		var buf bytes.Buffer
		must.NoError(t, b.Write(&buf))
		return buf.Bytes()
	}
	data := build()
	// Bundles are reproducible.
	must.Eq(t, data, build())

	file := filepath.Join(t.TempDir(), "packs.tar")
	must.NoError(t, os.WriteFile(file, data, 0o644))

	idx, err := ReadIndex(file)
	must.NoError(t, err)
	must.Len(t, 2, idx.Packs)
	must.Eq(t, parent.Digest(), idx.Packs[0].Digest)
	must.Eq(t, child.Digest(), idx.Packs[1].Digest)
	must.Eq(t, []string{"parent"}, idx.Packs[1].DependencyOf)

	// The air-gapped side: the source registry is gone.
	src.Close()

	b, err := Open(file)
	must.NoError(t, err)
	defer b.Close()

	policy := packconfig.Policy{RequireSignature: true, KeyFiles: []string{keyFile}}

	// Import into the cache.
	cache := t.TempDir()
	results, err := b.ImportToCache(ctx, cache, "offline", policy)
	must.NoError(t, err)
	must.Len(t, 2, results)
	for _, r := range results {
		must.True(t, r.Signature.Verified)
	}
	dir := supplychain.PackDir(cache, "offline", "parent", "1.0.0")
	_, err = supplychain.VerifyCached(dir, policy)
	must.NoError(t, err)

	// Push into an internal registry; the signatures stay valid.
	internal := ocitest.New().Start()
	defer internal.Close()
	base := oci.Location{Host: internal.Host(), Repository: "mirror"}
	results, err = b.Push(ctx, base, plain, policy)
	must.NoError(t, err)
	must.Len(t, 2, results)
	must.Eq(t, []string{"1.0.0", "sha256-" + parent.Digest()[len("sha256:"):] + ".sig"}, internal.Tags("mirror/parent"))

	repo, err := plain.Repository(base.Child("child"))
	must.NoError(t, err)
	sigs, err := oci.FetchSignatures(ctx, repo, child.Digest())
	must.NoError(t, err)
	pub, err := signing.LoadPublicKey(keyFile)
	must.NoError(t, err)
	must.True(t, signing.Verify(child.Digest(), sigs, []*signing.PublicKey{pub}).Verified)

	// A policy with another key rejects the bundle.
	other, err := signing.GenerateKeyPair(nil)
	must.NoError(t, err)
	otherFile := filepath.Join(t.TempDir(), "other.pub")
	must.NoError(t, os.WriteFile(otherFile, other.PublicKeyPEM, 0o644))
	_, err = b.ImportToCache(ctx, t.TempDir(), "offline", packconfig.Policy{RequireSignature: true, KeyFiles: []string{otherFile}})
	must.ErrorContains(t, err, "trust policy rejects")
}

func TestBundle_LocalUnsignedAndInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := testfixture.Clone(t, "v2/test_registry/packs/simple_raw_exec")
	p, err := loader.Load(dir)
	must.NoError(t, err)
	a, err := oci.Build(dir, p.Metadata)
	must.NoError(t, err)

	b, err := NewBuilder()
	must.NoError(t, err)
	defer b.Close()
	e, err := b.AddArtifact(ctx, a, "0.0.1", "dir:simple_raw_exec", nil)
	must.NoError(t, err)
	must.False(t, e.Signed)
	_, err = b.AddArtifact(ctx, a, "0.0.1", "dir:simple_raw_exec", nil)
	must.ErrorContains(t, err, "twice")

	file := filepath.Join(t.TempDir(), "b.tar")
	must.NoError(t, b.WriteFile(file))
	bundle, err := Open(file)
	must.NoError(t, err)
	defer bundle.Close()

	// Unsigned packs are refused when signatures are required, and accepted
	// otherwise.
	keyPair, err := signing.GenerateKeyPair(nil)
	must.NoError(t, err)
	keyFile := filepath.Join(t.TempDir(), "k.pub")
	must.NoError(t, os.WriteFile(keyFile, keyPair.PublicKeyPEM, 0o644))
	_, err = bundle.ImportToCache(ctx, t.TempDir(), "x", packconfig.Policy{RequireSignature: true, KeyFiles: []string{keyFile}})
	must.ErrorContains(t, err, "not signed")
	_, err = bundle.ImportToCache(ctx, t.TempDir(), "x", packconfig.Policy{})
	must.NoError(t, err)

	// Not a bundle.
	notBundle := filepath.Join(t.TempDir(), "x.tar")
	must.NoError(t, os.WriteFile(notBundle, []byte("garbage"), 0o644))
	_, err = Open(notBundle)
	must.Error(t, err)
}
