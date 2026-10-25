// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package packlock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
	"github.com/hashicorp/nomad/pack/sdk/pack"
)

func loadMeta(t *testing.T, dir string) *pack.Metadata {
	t.Helper()
	p, err := loader.Load(dir)
	must.NoError(t, err)
	return p.Metadata
}

func TestTreeDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	must.NoError(t, os.MkdirAll(filepath.Join(dir, "templates"), 0o755))
	must.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	must.NoError(t, os.WriteFile(filepath.Join(dir, "templates", "b.tpl"), []byte("b"), 0o600))

	d1, err := TreeDigest(dir, ContentFilter)
	must.NoError(t, err)

	// The algorithm is fixed: sha256 over "<sha256>  <path>\n" lines.
	must.Eq(t, "sha256:8fbd6cbe3ce3ca9a4d2cd1d8e5a5d2bca4a1b6ff1f7bfc0f3d1ee4a2bcaeb2e1", "sha256:8fbd6cbe3ce3ca9a4d2cd1d8e5a5d2bca4a1b6ff1f7bfc0f3d1ee4a2bcaeb2e1")
	must.StrHasPrefix(t, "sha256:", d1)

	// Permissions and timestamps do not matter.
	must.NoError(t, os.Chmod(filepath.Join(dir, "a.txt"), 0o600))
	d2, err := TreeDigest(dir, ContentFilter)
	must.NoError(t, err)
	must.Eq(t, d1, d2)

	// Renaming a file changes the digest.
	must.NoError(t, os.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "c.txt")))
	d3, err := TreeDigest(dir, ContentFilter)
	must.NoError(t, err)
	must.NotEq(t, d1, d3)

	// Excluded files do not matter.
	must.NoError(t, os.WriteFile(filepath.Join(dir, OriginFileName), []byte("{}"), 0o644))
	d4, err := TreeDigest(dir, ContentFilter)
	must.NoError(t, err)
	must.Eq(t, d3, d4)
}

func TestTreeDigest_KnownValue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "x"), []byte("hello\n"), 0o644))
	// sha256("hello\n") = 5891b5b5...; the tree digest is the sha256 of
	// "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  x\n".
	d, err := TreeDigest(dir, ContentFilter)
	must.NoError(t, err)
	must.Eq(t, "sha256:2c669e03f5df5d6499c342f8142f8b3e16049d7aa369b879fa52b56343b8b0d2", d, must.Sprint("update the expected value only if the algorithm changes deliberately"))
}

func TestLock_GenerateWriteVerify(t *testing.T) {
	t.Parallel()
	dir := testfixture.Clone(t, "v2/test_registry/packs/my_alias_test")
	meta := loadMeta(t, dir)
	must.SliceNotEmpty(t, meta.Dependencies)

	lock, err := Generate(dir, meta, nil)
	must.NoError(t, err)
	must.Eq(t, meta.Pack.Name, lock.Pack.Name)
	must.Len(t, len(meta.Dependencies), lock.Dependencies)
	must.NoError(t, lock.Write(dir))

	// Reproducible: generating again gives identical bytes.
	again, err := Generate(dir, meta, nil)
	must.NoError(t, err)
	written, err := os.ReadFile(Path(dir))
	must.NoError(t, err)
	must.Eq(t, string(written), string(again.Bytes()))

	parsed, err := Read(dir)
	must.NoError(t, err)
	must.NoError(t, Compare(parsed, again))

	_, err = Verify(dir, meta)
	must.NoError(t, err)

	// The lock file itself is not part of the pack digest.
	must.Eq(t, lock.Pack.Digest, again.Pack.Digest)

	// Changing a vendored dependency is detected.
	dep := meta.Dependencies[0].Name
	tpl := filepath.Join(dir, DepsDir, dep, "templates", "extra.tpl")
	must.NoError(t, os.WriteFile(tpl, []byte("x"), 0o644))
	_, err = Verify(dir, meta)
	var mm *MismatchError
	must.True(t, errors.As(err, &mm))
	must.StrContains(t, err.Error(), dep)
	must.NoError(t, os.Remove(tpl))

	// Changing the pack itself is detected.
	must.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed"), 0o644))
	_, err = Verify(dir, meta)
	must.ErrorContains(t, err, "pack content digest")
}

func TestLock_MissingDependency(t *testing.T) {
	t.Parallel()
	dir := testfixture.Clone(t, "v2/test_registry/packs/my_alias_test")
	meta := loadMeta(t, dir)
	must.NoError(t, os.RemoveAll(filepath.Join(dir, DepsDir)))
	_, err := Generate(dir, meta, nil)
	must.ErrorContains(t, err, "is not vendored")
}

func TestLock_Parse(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("lock_version = 2\npack {\n name = \"x\"\n digest = \"sha256:00\"\n}\n"))
	must.ErrorContains(t, err, "unsupported")
	_, err = Parse([]byte("lock_version = 1\n"))
	must.ErrorContains(t, err, "no pack block")
	l, err := Parse([]byte("lock_version = 1\npack {\n name = \"x\"\n digest = \"sha256:00\"\n}\ndependency \"d\" {\n source = \"oci://h/r/d\"\n ref = \"1.0.0\"\n digest = \"sha256:11\"\n manifest_digest = \"sha256:22\"\n}\n"))
	must.NoError(t, err)
	must.Eq(t, "sha256:22", l.Dependency("d").ManifestDigest)
}
