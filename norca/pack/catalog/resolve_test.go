// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shoenig/test/must"
)

// notPlainNames are names that would leave a directory, or are a path, on
// Unix or on Windows. Every one is refused on both.
var notPlainNames = []string{
	"..", ".", "../x", "../../other/main/evil", "a/b", "/abs", "/",
	`..\x`, `a\b`, `\abs`, `\\host\share`, `C:\Windows`, `C:x`, "c:", "pack:stream",
	"a\x00b", "a\nb", "a\tb", strings.Repeat("a", 256),
}

func TestValidatePackNames(t *testing.T) {
	for _, name := range notPlainNames {
		must.Error(t, ValidatePackName(name), must.Sprintf("pack %q", name))
		must.Error(t, ValidatePackVersion(name), must.Sprintf("version %q", name))
		must.Error(t, ValidateRegistryName(name), must.Sprintf("registry %q", name))
		must.Error(t, ValidateRef("dir", name), must.Sprintf("dir ref %q", name))
		must.Error(t, ValidateRef("oci", name), must.Sprintf("oci ref %q", name))
	}
	must.Error(t, ValidatePackName(""))
	must.Error(t, ValidateRegistryName(""))
	must.Error(t, ValidatePackName("redis@latest"))
	must.NoError(t, ValidatePackVersion(""))

	for _, name := range []string{"redis", "loadbalancer-operator", "My_Pack.v2", "0.3.0", "v1.0.0+build.5", "latest", "a..b", "pack!_x"} {
		must.NoError(t, ValidatePackName(name), must.Sprint(name))
		must.NoError(t, ValidatePackVersion(name), must.Sprint(name))
		must.NoError(t, ValidateRef("dir", name), must.Sprint(name))
		must.NoError(t, ValidateRef("git", name), must.Sprint(name))
	}

	// A git ref may be namespaced; the cache escapes the "/". Nothing else
	// of a path is taken, and no ref is an option of a command.
	must.NoError(t, ValidateRef("git", "release/1.0"))
	must.NoError(t, ValidateRef("git", "refs/tags/v1"))
	must.Error(t, ValidateRef("dir", "release/1.0"))
	for _, ref := range []string{"..", ".", `..\x`, `a\b`, `C:\x`, "c:", "", "--upload-pack=x", "-b"} {
		must.Error(t, ValidateRef("git", ref), must.Sprintf("git ref %q", ref))
	}
	must.Error(t, ValidateRef("dir", "-rf"))
}

// TestFindPackDir checks that a pack is only ever found in the cache of its
// registry, by the names of the directories that are there.
func TestFindPackDir(t *testing.T) {
	cache := t.TempDir()
	mk := func(parts ...string) string {
		dir := filepath.Join(append([]string{cache}, parts...)...)
		must.NoError(t, os.MkdirAll(dir, 0o755))
		must.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.hcl"), []byte("pack {}\n"), 0o644))
		return dir
	}
	latest := mk("approved", "latest", "redis@latest")
	v2 := mk("approved", "2.0.0", "redis@2.0.0")
	mk("approved", "latest", "web@latest")
	mk("approved", ".staging", "hidden@.staging")
	mk("other", "latest", "evil@latest")
	mk("other", "main", "redis@main")
	outside := t.TempDir()
	must.NoError(t, os.MkdirAll(filepath.Join(outside, "secret@latest"), 0o755))

	find := func(pack, version string, anyRef bool) (string, error) {
		return FindPackDir(PackLookup{CacheDir: cache, Registry: "approved", Pack: pack, Version: version,
			Refs: []string{"latest"}, AnyRef: anyRef})
	}

	// By ref, by the directory of a version, by any directory of the pack.
	dir, err := find("redis", "latest", false)
	must.NoError(t, err)
	must.Eq(t, latest, dir)
	dir, err = find("redis", "2.0.0", false)
	must.NoError(t, err)
	must.Eq(t, v2, dir)
	dir, err = find("redis", "", false)
	must.NoError(t, err)
	must.Eq(t, v2, dir)
	// A version the catalog lists from the metadata of the pack of a ref.
	dir, err = find("web", "0.3.0", true)
	must.NoError(t, err)
	must.Eq(t, filepath.Join(cache, "approved", "latest", "web@latest"), dir)
	// Without a ref to look in, such a version needs the catalog's word.
	lookup := PackLookup{CacheDir: cache, Registry: "approved", Pack: "web", Version: "0.3.0"}
	_, err = FindPackDir(lookup)
	must.ErrorIs(t, err, ErrPackNotCached)
	lookup.AnyRef = true
	dir, err = FindPackDir(lookup)
	must.NoError(t, err)
	must.Eq(t, filepath.Join(cache, "approved", "latest", "web@latest"), dir)
	// An asked version that is no directory: the refs of the registry.
	dir, err = find("redis", "9.9.9", false)
	must.NoError(t, err)
	must.Eq(t, latest, dir)

	// Not in the cache of this registry.
	for _, q := range [][2]string{{"evil", ""}, {"evil", "latest"}, {"redis", "main"}, {"nope", ""}, {"hidden", ".staging"}} {
		if q[0] == "redis" && q[1] == "main" {
			// "main" is no directory of the registry; its refs still serve.
			dir, err = find(q[0], q[1], false)
			must.NoError(t, err)
			must.Eq(t, latest, dir)
			continue
		}
		_, err = find(q[0], q[1], false)
		must.ErrorIs(t, err, ErrPackNotCached, must.Sprint(q))
	}
	_, err = FindPackDir(PackLookup{CacheDir: cache, Registry: "missing", Pack: "redis"})
	must.ErrorIs(t, err, ErrPackNotCached)

	// Names that are paths are refused, whatever is at the path: the pack of
	// another registry, a directory outside of the cache.
	rel, err := filepath.Rel(filepath.Join(cache, "approved", "latest"), filepath.Join(outside, "secret"))
	must.NoError(t, err)
	for _, q := range [][2]string{
		{"../../other/latest/evil", "latest"},
		{"../../other/main/redis", "main"},
		{"redis", "../../other/main"},
		{rel, "latest"},
		{filepath.Join(outside, "secret"), "latest"},
		{`..\..\other\latest\evil`, "latest"},
		{"redis", `..\..\other\main`},
		{"redis", ".."},
	} {
		dir, err = find(q[0], q[1], true)
		must.Error(t, err, must.Sprint(q, dir))
		// Refused as a name, before the cache is looked at.
		must.False(t, errors.Is(err, ErrPackNotCached), must.Sprint(q, err))
	}
	for _, registry := range []string{"..", ".", "approved/latest", `approved\latest`, "../" + filepath.Base(outside)} {
		_, err = FindPackDir(PackLookup{CacheDir: cache, Registry: registry, Pack: "redis"})
		must.Error(t, err, must.Sprint(registry))
	}

	// A symbolic link in the cache is not a ref or a pack directory, where
	// ever it points.
	if runtime.GOOS != "windows" {
		must.NoError(t, os.Symlink(filepath.Join(cache, "other", "latest"), filepath.Join(cache, "approved", "linked")))
		must.NoError(t, os.Symlink(outside, filepath.Join(cache, "approved", "out")))
		must.NoError(t, os.Symlink(filepath.Join(cache, "other", "latest", "evil@latest"),
			filepath.Join(cache, "approved", "latest", "evil@latest")))
		for _, q := range [][2]string{{"evil", "linked"}, {"evil", ""}, {"evil", "latest"}, {"secret", "out"}, {"secret", ""}} {
			_, err = find(q[0], q[1], true)
			must.ErrorIs(t, err, ErrPackNotCached, must.Sprint(q))
		}
	}
}
