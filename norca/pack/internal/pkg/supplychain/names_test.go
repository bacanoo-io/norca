// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package supplychain

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"
)

// TestValidateTag checks the tags accepted as cache path elements (PK-1).
func TestValidateTag(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{"1.0.0", "v0.1.0-rc.1", "latest", "_x", "A_b-c.d", "sha256-abc.sig", "1..2"} {
		must.NoError(t, ValidateTag(tag), must.Sprint(tag))
	}
	long := ""
	for len(long) < 129 {
		long += "a"
	}
	for _, tag := range []string{
		"", ".", "..", "../x", `..\x`, "a/b", `a\b`, "/abs", `\abs`, `\\server\share`,
		"C:", `C:\x`, "c:x", ".hidden", "-x", "a b", "a:b", "a@b", "a+b", "tag\x00", "x\n",
		"CON", "con", "NUL.txt", "com1", "LPT9.tar", "COM¹", "aux", "1.0.", long,
	} {
		must.Error(t, ValidateTag(tag), must.Sprintf("%q", tag))
	}
}

// TestValidatePathName checks pack and cache registry names (PK-1).
func TestValidatePathName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"hello_world", "my-pack", "pack.v2", "offline", "Norca Packs"} {
		must.NoError(t, ValidatePathName("pack name", name), must.Sprint(name))
	}
	for _, name := range []string{
		"", ".", "..", "a/b", `a\b`, "/abs", `\abs`, `\\server\share`, "C:", `C:\x`, "c:x",
		"a@1.0", "a*", "a?", `a"`, "a<b", "a>b", "a|b", "a\x01", "CON", "prn.txt", "Lpt3",
		"conin$", "x.", "x ",
	} {
		err := ValidatePathName("pack name", name)
		must.ErrorContains(t, err, "invalid pack name", must.Sprintf("%q", name))
	}
}

// TestFetch_InvalidNames checks that Fetch refuses names that would leave
// the cache before it touches the source or the file system.
func TestFetch_InvalidNames(t *testing.T) {
	t.Parallel()
	cache := filepath.Join(t.TempDir(), "cache")
	cases := []struct{ registry, pack, tag, err string }{
		{"offline", "app", `..\..\victim`, "invalid tag"},
		{"offline", "app", "../victim", "invalid tag"},
		{"..", "app", "1.0.0", "invalid cache registry name"},
		{"offline", `..\app`, "1.0.0", "invalid pack name"},
	}
	for _, c := range cases {
		// A nil source: Fetch must fail before using it.
		_, _, err := Fetch(context.Background(), nil, c.pack, c.tag, FetchOptions{CacheRoot: cache, Registry: c.registry})
		must.ErrorContains(t, err, c.err)
	}
}

func TestWithin(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cache", "offline")
	must.True(t, within(root, filepath.Join(root, "1.0.0", "app@1.0.0")))
	must.True(t, within(root, root))
	must.False(t, within(root, filepath.Join(root, "..", "x")))
	must.False(t, within(root, filepath.Join(root, "..")))
	must.False(t, within(root, filepath.Dir(root)))
}
