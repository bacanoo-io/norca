// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package caching

import (
	"os"
	"path"
	"testing"

	"github.com/shoenig/test/must"
)

// cachePackAt creates the cache directory of a pack at a ref.
func cachePackAt(t *testing.T, cacheDir, registry, pack, ref string) {
	t.Helper()
	escapedRef := EscapeRef(ref)
	dir := path.Join(cacheDir, registry, escapedRef, AppendRef(EscapePackName(pack), escapedRef))
	must.NoError(t, os.MkdirAll(dir, 0o700))
}

func writeDefaultRef(t *testing.T, cacheDir, registry, ref string) {
	t.Helper()
	must.NoError(t, os.WriteFile(path.Join(cacheDir, registry, DefaultRefFile), []byte(ref+"\n"), 0o644))
}

func TestDefaultPackRef(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		refs     []string // refs the pack is cached at
		other    []string // refs only another pack is cached at
		recorded string   // content of the default ref record
		expected string
	}{
		{name: "no registry", expected: DefaultRef},
		{name: "only latest", refs: []string{DefaultRef}, expected: DefaultRef},
		{name: "single pinned ref", refs: []string{"v0.1.0"}, expected: "v0.1.0"},
		{name: "single pinned ref with slash", refs: []string{"packs/v0.1.0"}, expected: "packs/v0.1.0"},
		{name: "recorded pinned ref", refs: []string{"v0.1.0"}, recorded: "v0.1.0", expected: "v0.1.0"},
		{name: "latest without record", refs: []string{DefaultRef, "v0.1.0"}, expected: DefaultRef},
		{name: "recorded ref wins over latest", refs: []string{DefaultRef, "v0.1.0"}, recorded: "v0.1.0", expected: "v0.1.0"},
		{name: "recorded latest", refs: []string{DefaultRef, "v0.1.0"}, recorded: DefaultRef, expected: DefaultRef},
		{name: "recorded ref without the pack", refs: []string{"v0.1.0"}, other: []string{"v0.2.0"}, recorded: "v0.2.0", expected: "v0.1.0"},
		{name: "several pinned refs", refs: []string{"v0.1.0", "v0.2.0"}, expected: DefaultRef},
		{name: "recorded among several pinned refs", refs: []string{"v0.1.0", "v0.2.0"}, recorded: "v0.1.0", expected: "v0.1.0"},
		{name: "only another pack", other: []string{"v0.1.0"}, expected: DefaultRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cacheDir := t.TempDir()
			for _, ref := range tc.refs {
				cachePackAt(t, cacheDir, "reg", "MyPack", ref)
			}
			for _, ref := range tc.other {
				cachePackAt(t, cacheDir, "reg", "other", ref)
			}
			if tc.recorded != "" {
				must.NoError(t, os.MkdirAll(path.Join(cacheDir, "reg"), 0o700))
				writeDefaultRef(t, cacheDir, "reg", tc.recorded)
			}
			must.Eq(t, tc.expected, DefaultPackRef(cacheDir, "reg", "MyPack"))
		})
	}
}

func TestPackConfigInit_DefaultRef(t *testing.T) {
	// Not parallel: sets the cache directory of the process.
	cacheDir := t.TempDir()
	t.Setenv(EnvCacheDir, cacheDir)
	cachePackAt(t, cacheDir, "norca", "nats", "v0.1.0")

	// Without a ref the pack is used at the only cached ref.
	cfg := &PackConfig{Registry: "norca", Name: "nats"}
	cfg.Init()
	must.True(t, cfg.RefDefaulted)
	must.Eq(t, "v0.1.0", cfg.Ref)
	must.Eq(t, path.Join(DefaultCachePath(), "norca", "v0.1.0", "nats@v0.1.0"), cfg.Path)
	_, err := os.Stat(cfg.Path)
	must.NoError(t, err)

	// An explicit ref is kept, also if the pack is not cached at it.
	cfg = &PackConfig{Registry: "norca", Name: "nats", Ref: DefaultRef}
	cfg.Init()
	must.False(t, cfg.RefDefaulted)
	must.Eq(t, DefaultRef, cfg.Ref)
	must.Eq(t, path.Join(DefaultCachePath(), "norca", "latest", "nats@latest"), cfg.Path)

	cfg = &PackConfig{Registry: "norca", Name: "nats", Ref: "v0.2.0"}
	cfg.Init()
	must.False(t, cfg.RefDefaulted)
	must.Eq(t, "v0.2.0", cfg.Ref)

	// A pack that is not cached keeps the default ref "latest".
	cfg = &PackConfig{Registry: "norca", Name: "missing"}
	cfg.Init()
	must.True(t, cfg.RefDefaulted)
	must.Eq(t, DefaultRef, cfg.Ref)
}

func TestAddRegistryRecordsDefaultRef(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	cache, err := NewCache(&CacheConfig{
		Path:   cacheDir,
		Logger: NewTestLogger(t),
	})
	must.NoError(t, err)

	// Added at a pinned ref: the packs are used at it by default.
	_, err = cache.Add(&AddOpts{
		RegistryName: "pinned",
		Source:       tReg.SourceURL(),
		Ref:          tReg.Ref1(),
	})
	must.NoError(t, err)
	b, err := os.ReadFile(path.Join(cacheDir, "pinned", DefaultRefFile))
	must.NoError(t, err)
	must.Eq(t, tReg.Ref1()+"\n", string(b))
	must.Eq(t, tReg.Ref1(), DefaultPackRef(cacheDir, "pinned", "simple_raw_exec"))

	// Added again at latest: latest becomes the default.
	_, err = cache.Add(testAddOpts("pinned"))
	must.NoError(t, err)
	must.Eq(t, DefaultRef, DefaultPackRef(cacheDir, "pinned", "simple_raw_exec"))

	// Added again at the pinned ref: the pinned ref is the default again,
	// although the packs are also cached at latest.
	_, err = cache.Add(&AddOpts{
		RegistryName: "pinned",
		Source:       tReg.SourceURL(),
		Ref:          tReg.Ref1(),
	})
	must.NoError(t, err)
	must.Eq(t, tReg.Ref1(), DefaultPackRef(cacheDir, "pinned", "simple_raw_exec"))

	// The record file does not disturb loading the cache.
	loaded, err := NewCache(&CacheConfig{
		Path:   cacheDir,
		Logger: NewTestLogger(t),
	})
	must.NoError(t, err)
	must.NoError(t, loaded.Load())
	must.Len(t, 2, loaded.Registries())
}
