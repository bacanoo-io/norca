// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci/ocitest"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
)

const testPack = "v2/test_registry/packs/simple_raw_exec"

func buildTestArtifact(t *testing.T) (*Artifact, string) {
	t.Helper()
	dir := testfixture.Clone(t, testPack)
	p, err := loader.Load(dir)
	must.NoError(t, err)
	a, err := Build(dir, p.Metadata)
	must.NoError(t, err)
	return a, dir
}

func TestParseLocation(t *testing.T) {
	t.Parallel()
	loc, err := ParseLocation("oci://registry.example.com:5000/team/packs")
	must.NoError(t, err)
	must.Eq(t, "registry.example.com:5000", loc.Host)
	must.Eq(t, "team/packs", loc.Repository)
	must.Eq(t, "oci://registry.example.com:5000/team/packs/redis:0.2.1", loc.Child("redis").WithReference("0.2.1").String())
	must.Eq(t, "registry.example.com:5000/team/packs/redis", loc.Child("redis").RepositoryName())

	loc, err = ParseLocation("oci://localhost:5000")
	must.NoError(t, err)
	must.Eq(t, "localhost:5000/redis", loc.Child("redis").RepositoryName())

	loc, err = ParseLocation("oci://ghcr.io/org/redis@sha256:1111111111111111111111111111111111111111111111111111111111111111")
	must.NoError(t, err)
	must.Eq(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", loc.Reference)

	_, err = ParseLocation("github.com/org/repo")
	must.Error(t, err)
}

func TestBuild_Reproducible(t *testing.T) {
	t.Parallel()
	a, dir := buildTestArtifact(t)

	// A copy of the pack with different timestamps produces the same digest.
	other := testfixture.Clone(t, testPack)
	must.NoError(t, os.Chtimes(filepath.Join(other, "metadata.hcl"), a0, a0))
	p, err := loader.Load(other)
	must.NoError(t, err)
	b, err := Build(other, p.Metadata)
	must.NoError(t, err)
	must.Eq(t, a.Digest(), b.Digest())

	// The origin record and version control data are not part of the content.
	must.NoError(t, os.WriteFile(filepath.Join(other, packlock.OriginFileName), []byte("{}"), 0o644))
	must.NoError(t, os.MkdirAll(filepath.Join(other, ".git"), 0o755))
	must.NoError(t, os.WriteFile(filepath.Join(other, ".git", "HEAD"), []byte("x"), 0o644))
	b, err = Build(other, p.Metadata)
	must.NoError(t, err)
	must.Eq(t, a.Digest(), b.Digest())

	// Changing the content changes the digest.
	must.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed"), 0o644))
	c, err := Build(dir, p.Metadata)
	must.NoError(t, err)
	must.NotEq(t, a.Digest(), c.Digest())

	var m ocispec.Manifest
	must.NoError(t, json.Unmarshal(a.Manifest, &m))
	must.Eq(t, ArtifactType, m.ArtifactType)
	must.Eq(t, ConfigMediaType, m.Config.MediaType)
	must.Eq(t, ContentMediaType, m.Layers[0].MediaType)
	must.Eq(t, "simple_raw_exec", m.Annotations[AnnotationPackName])
	must.StrHasPrefix(t, "sha256:", m.Annotations[AnnotationTreeDigest])
	must.MapNotContainsKey(t, m.Annotations, ocispec.AnnotationCreated)
}

var a0 = mustTime()

func TestPushPull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := ocitest.New().Start()
	defer srv.Close()

	a, src := buildTestArtifact(t)
	loc := Location{Host: srv.Host(), Repository: "packs/simple_raw_exec"}
	opts := &Options{PlainHTTP: true, NoDockerConfig: true}
	repo, err := opts.Repository(loc)
	must.NoError(t, err)
	must.NoError(t, a.Push(ctx, repo, "0.0.1"))
	// Pushing again is idempotent.
	must.NoError(t, a.Push(ctx, repo, "0.0.1"))
	must.Eq(t, []string{"0.0.1"}, srv.Tags("packs/simple_raw_exec"))

	got, err := Fetch(ctx, repo, "0.0.1")
	must.NoError(t, err)
	must.Eq(t, a.Digest(), got.Digest())
	must.Eq(t, "simple_raw_exec", got.Config.Name)

	dest := filepath.Join(t.TempDir(), "out")
	must.NoError(t, got.Extract(dest))
	want, err := packlock.TreeDigest(src, packlock.ContentFilter)
	must.NoError(t, err)
	have, err := packlock.TreeDigest(dest, packlock.ContentFilter)
	must.NoError(t, err)
	must.Eq(t, want, have)

	// By digest.
	got, err = Fetch(ctx, repo, a.Digest())
	must.NoError(t, err)
	must.Eq(t, a.Digest(), got.Digest())

	_, err = Fetch(ctx, repo, "9.9.9")
	must.Error(t, err)
}

func TestPull_RejectsNonPack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := ocitest.New()
	srv := reg.Start()
	defer srv.Close()

	cfg := reg.PutBlob([]byte("{}"))
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + cfg + `","size":2},"layers":[]}`)
	reg.PutManifest("packs/image", "1.0.0", ocispec.MediaTypeImageManifest, manifest)

	repo, err := (&Options{PlainHTTP: true, NoDockerConfig: true}).Repository(Location{Host: srv.Host(), Repository: "packs/image"})
	must.NoError(t, err)
	_, err = Fetch(ctx, repo, "1.0.0")
	must.ErrorContains(t, err, "is not a Norca pack")
}

func TestExtract_TamperedContent(t *testing.T) {
	t.Parallel()
	a, _ := buildTestArtifact(t)

	// Content that does not match the signed tree digest is rejected.
	a.content = tarGz(t, map[string]string{"metadata.hcl": "pack { name = \"x\" }"})
	dest := filepath.Join(t.TempDir(), "out")
	must.ErrorContains(t, a.Extract(dest), "does not match the manifest")
	_, err := os.Stat(dest)
	must.True(t, os.IsNotExist(err))
}

func TestExtract_PathTraversal(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../evil", "/abs/evil", "a/../../evil", `..\evil`} {
		data := tarGz(t, map[string]string{name: "x"})
		err := unarchive(data, filepath.Join(t.TempDir(), "out"))
		must.ErrorContains(t, err, "invalid path", must.Sprintf("name %q", name))
	}

	// Symbolic links are rejected.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must.NoError(t, tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}))
	must.NoError(t, tw.Close())
	must.NoError(t, gz.Close())
	must.ErrorContains(t, unarchive(buf.Bytes(), filepath.Join(t.TempDir(), "out")), "unsupported entry")
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		must.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}))
		_, err := tw.Write([]byte(content))
		must.NoError(t, err)
	}
	must.NoError(t, tw.Close())
	must.NoError(t, gz.Close())
	return buf.Bytes()
}

func TestAuthAndTLS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := ocitest.New()
	reg.Username, reg.Password = "user", "pass"
	srv := reg.StartTLS()
	defer srv.Close()

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	must.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644))

	a, _ := buildTestArtifact(t)
	loc := Location{Host: srv.Host(), Repository: "packs/simple_raw_exec"}

	// Without the CA the certificate is rejected.
	repo, err := (&Options{Username: "user", Password: "pass", NoDockerConfig: true}).Repository(loc)
	must.NoError(t, err)
	must.ErrorContains(t, a.Push(ctx, repo, "0.0.1"), "certificate")

	// Without credentials the registry refuses.
	repo, err = (&Options{CACertFile: caFile, NoDockerConfig: true}).Repository(loc)
	must.NoError(t, err)
	must.Error(t, a.Push(ctx, repo, "0.0.1"))

	repo, err = (&Options{CACertFile: caFile, Username: "user", Password: "pass", NoDockerConfig: true}).Repository(loc)
	must.NoError(t, err)
	must.NoError(t, a.Push(ctx, repo, "0.0.1"))

	// Credentials from the environment are not tested here, as the test
	// runs in parallel; see the CLI tests.
	repo, err = (&Options{TLSSkipVerify: true, Username: "user", Password: "pass", NoDockerConfig: true}).Repository(loc)
	must.NoError(t, err)
	_, err = Fetch(ctx, repo, "0.0.1")
	must.NoError(t, err)
}

func TestSignatures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := ocitest.New()
	srv := reg.Start()
	defer srv.Close()

	a, _ := buildTestArtifact(t)
	loc := Location{Host: srv.Host(), Repository: "packs/simple_raw_exec"}
	repo, err := (&Options{PlainHTTP: true, NoDockerConfig: true}).Repository(loc)
	must.NoError(t, err)
	must.NoError(t, a.Push(ctx, repo, "0.0.1"))

	sigs, err := FetchSignatures(ctx, repo, a.Digest())
	must.NoError(t, err)
	must.SliceEmpty(t, sigs)

	key1, pub1 := testKey(t)
	key2, pub2 := testKey(t)
	sign := func(key crypto.Signer) {
		payload, err := signing.NewPayload(loc.RepositoryName(), a.Digest())
		must.NoError(t, err)
		sig, err := signing.Sign(key, payload)
		must.NoError(t, err)
		must.NoError(t, AttachSignature(ctx, repo, a.Digest(), signing.Signature{Payload: payload, Signature: sig}, key.Public()))
	}
	sign(key1)
	sign(key2)
	// Re-signing with a key replaces its previous signature.
	sign(key1)

	sigs, err = FetchSignatures(ctx, repo, a.Digest())
	must.NoError(t, err)
	must.Len(t, 2, sigs)

	res := signing.Verify(a.Digest(), sigs, []*signing.PublicKey{pub2})
	must.True(t, res.Verified)
	must.Eq(t, pub2.ID, res.KeyID)
	res = signing.Verify(a.Digest(), sigs, []*signing.PublicKey{pub1})
	must.True(t, res.Verified)

	// The signature manifest has the layout cosign expects.
	tag, err := SignatureTag(a.Digest())
	must.NoError(t, err)
	must.StrHasSuffix(t, ".sig", tag)
	raw, ok := reg.Manifest("packs/simple_raw_exec", tag)
	must.True(t, ok)
	var m ocispec.Manifest
	must.NoError(t, json.Unmarshal(raw, &m))
	must.Eq(t, ocispec.MediaTypeImageManifest, m.MediaType)
	must.Eq(t, ocispec.MediaTypeImageConfig, m.Config.MediaType)
	for _, l := range m.Layers {
		must.Eq(t, signing.PayloadMediaType, l.MediaType)
		must.MapContainsKey(t, l.Annotations, signing.SignatureAnnotation)
	}
}

func testKey(t *testing.T) (crypto.Signer, *signing.PublicKey) {
	t.Helper()
	kp, err := signing.GenerateKeyPair(nil)
	must.NoError(t, err)
	priv, err := signing.DecryptPrivateKey(kp.PrivateKeyPEM, nil)
	must.NoError(t, err)
	pub, err := signing.ParsePublicKey(kp.PublicKeyPEM)
	must.NoError(t, err)
	id, err := signing.KeyID(pub)
	must.NoError(t, err)
	return priv, &signing.PublicKey{Key: pub, ID: id}
}

func mustTime() time.Time {
	return time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
}
