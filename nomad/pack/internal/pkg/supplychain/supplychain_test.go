// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package supplychain

import (
	"context"
	"crypto"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci/ocitest"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
)

func TestNewestTag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tags []string
		want string
		err  bool
	}{
		{[]string{"0.1.0", "0.10.0", "0.9.1", "sha256-ab.sig", "latest"}, "0.10.0", false},
		{[]string{"1.0.0-beta.1", "0.9.0"}, "0.9.0", false},
		{[]string{"1.0.0-beta.1", "1.0.0-beta.2"}, "1.0.0-beta.2", false},
		{[]string{"latest", "main"}, "latest", false},
		{[]string{"main"}, "", true},
		{nil, "", true},
	}
	for _, c := range cases {
		got, err := NewestTag(c.tags)
		if c.err {
			must.Error(t, err)
			continue
		}
		must.NoError(t, err)
		must.Eq(t, c.want, got)
	}
}

func TestMatchRegistry(t *testing.T) {
	t.Parallel()
	conf := &packconfig.Config{Registries: []*packconfig.Registry{
		{Name: "root", URL: "oci://reg.example.com"},
		{Name: "team", URL: "oci://reg.example.com/team"},
		{Name: "other", URL: "oci://other.example.com/team"},
	}}
	loc := func(s string) oci.Location {
		l, err := oci.ParseLocation(s)
		must.NoError(t, err)
		return l
	}
	must.Eq(t, "team", MatchRegistry(conf, loc("oci://reg.example.com/team/redis")).Name)
	must.Eq(t, "root", MatchRegistry(conf, loc("oci://reg.example.com/teams/redis")).Name)
	must.Nil(t, MatchRegistry(conf, loc("oci://third.example.com/team/redis")))
}

type fixture struct {
	srv      *ocitest.Server
	repo     oci.Target
	loc      oci.Location
	artifact *oci.Artifact
	key      crypto.Signer
	keyFile  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{srv: ocitest.New().Start()}
	t.Cleanup(f.srv.Close)

	dir := testfixture.Clone(t, "v2/test_registry/packs/simple_raw_exec")
	p, err := loader.Load(dir)
	must.NoError(t, err)
	f.artifact, err = oci.Build(dir, p.Metadata)
	must.NoError(t, err)

	f.loc = oci.Location{Host: f.srv.Host(), Repository: "packs/simple_raw_exec"}
	repo, err := (&oci.Options{PlainHTTP: true, NoDockerConfig: true}).Repository(f.loc)
	must.NoError(t, err)
	f.repo = repo
	must.NoError(t, f.artifact.Push(ctx, repo, "0.0.1"))

	kp, err := signing.GenerateKeyPair(nil)
	must.NoError(t, err)
	f.key, err = signing.DecryptPrivateKey(kp.PrivateKeyPEM, nil)
	must.NoError(t, err)
	f.keyFile = filepath.Join(t.TempDir(), "cosign.pub")
	must.NoError(t, os.WriteFile(f.keyFile, kp.PublicKeyPEM, 0o644))
	return f
}

func (f *fixture) sign(t *testing.T) {
	t.Helper()
	payload, err := signing.NewPayload(f.loc.RepositoryName(), f.artifact.Digest())
	must.NoError(t, err)
	sig, err := signing.Sign(f.key, payload)
	must.NoError(t, err)
	must.NoError(t, oci.AttachSignature(context.Background(), f.repo, f.artifact.Digest(),
		signing.Signature{Payload: payload, Signature: sig}, f.key.Public()))
}

func (f *fixture) source(t *testing.T) Source {
	t.Helper()
	repo, err := (&oci.Options{PlainHTTP: true, NoDockerConfig: true}).Repository(f.loc)
	must.NoError(t, err)
	return repo
}

func TestFetch_Policy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	cache := t.TempDir()
	opts := FetchOptions{
		CacheRoot: cache, Registry: "internal", RegistrySource: "oci://" + f.srv.Host() + "/packs",
		Location: f.loc.String(), Repository: f.loc.RepositoryName(),
		Policy: packconfig.Policy{RequireSignature: true, KeyFiles: []string{f.keyFile}},
	}

	// Unsigned packs are rejected and not cached.
	_, _, err := Fetch(ctx, f.source(t), "simple_raw_exec", "0.0.1", opts)
	var pe *PolicyError
	must.True(t, errors.As(err, &pe), must.Sprintf("error: %v", err))
	must.StrContains(t, err.Error(), "not signed")
	_, err = os.Stat(PackDir(cache, "internal", "simple_raw_exec", "0.0.1"))
	must.True(t, os.IsNotExist(err))

	// Requiring a signature without trusted keys is an error.
	noKeys := opts
	noKeys.Policy.KeyFiles = nil
	_, _, err = Fetch(ctx, f.source(t), "simple_raw_exec", "0.0.1", noKeys)
	must.ErrorContains(t, err, "no trusted public keys")

	// The pack name must match.
	_, _, err = Fetch(ctx, f.source(t), "other", "0.0.1", opts)
	must.ErrorContains(t, err, "expected \"other\"")

	f.sign(t)
	dir, res, err := Fetch(ctx, f.source(t), "simple_raw_exec", "0.0.1", opts)
	must.NoError(t, err)
	must.Eq(t, PackDir(cache, "internal", "simple_raw_exec", "0.0.1"), dir)
	must.Eq(t, f.artifact.Digest(), res.Digest)
	must.True(t, res.Signature.Verified)
	must.StrHasPrefix(t, "sha256:", res.SignerKeyID())

	// The cached pack loads and verifies offline.
	_, err = loader.Load(dir)
	must.NoError(t, err)
	again, err := VerifyCached(dir, opts.Policy)
	must.NoError(t, err)
	must.Eq(t, res.Digest, again.Digest)
	must.Eq(t, res.SignerKeyID(), again.SignerKeyID())

	// A policy with another key rejects the cached pack.
	otherKP, err := signing.GenerateKeyPair(nil)
	must.NoError(t, err)
	otherKey := filepath.Join(t.TempDir(), "other.pub")
	must.NoError(t, os.WriteFile(otherKey, otherKP.PublicKeyPEM, 0o644))
	_, err = VerifyCached(dir, packconfig.Policy{RequireSignature: true, KeyFiles: []string{otherKey}})
	must.ErrorContains(t, err, "no signature by a trusted key")

	// Modifying the cached pack is detected.
	must.NoError(t, os.WriteFile(filepath.Join(dir, "templates", "evil.nomad.tpl"), []byte("job \"x\" {}"), 0o644))
	_, err = VerifyCached(dir, opts.Policy)
	must.ErrorContains(t, err, "was modified after download")

	// Packs without an origin record have no result.
	res, err = VerifyCached(t.TempDir(), opts.Policy)
	must.NoError(t, err)
	must.Nil(t, res)
}

func TestFetch_ReplacesCachedVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	cache := t.TempDir()
	opts := FetchOptions{CacheRoot: cache, Registry: "r", Location: f.loc.String()}

	dir, _, err := Fetch(ctx, f.source(t), "simple_raw_exec", "0.0.1", opts)
	must.NoError(t, err)
	must.NoError(t, os.WriteFile(filepath.Join(dir, "stale"), []byte("x"), 0o644))
	dir, _, err = Fetch(ctx, f.source(t), "simple_raw_exec", "0.0.1", opts)
	must.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "stale"))
	must.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, packlock.OriginFileName))
	must.NoError(t, err)

	// No temporary directories are left behind.
	entries, err := os.ReadDir(filepath.Dir(dir))
	must.NoError(t, err)
	for _, e := range entries {
		must.NotEq(t, '.', rune(e.Name()[0]), must.Sprintf("leftover %s", e.Name()))
	}
}

func TestUnpack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.sign(t)
	ep := &Endpoint{
		Location: f.loc,
		Options:  &oci.Options{PlainHTTP: true, NoDockerConfig: true},
		Policy:   packconfig.Policy{RequireSignature: true, KeyFiles: []string{f.keyFile}},
	}
	dest := filepath.Join(t.TempDir(), "deps", "simple_raw_exec")
	res, err := Unpack(ctx, ep, "", dest)
	must.NoError(t, err)
	must.Eq(t, f.artifact.Digest(), res.Digest)
	must.Eq(t, f.loc.WithReference("0.0.1").String(), res.Source)
	_, err = os.Stat(filepath.Join(dest, "metadata.hcl"))
	must.NoError(t, err)
}
