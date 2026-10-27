// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
)

// craftedTags are tags a bundle author could use to reach a path outside of
// the pack cache, on any OS.
var craftedTags = []string{
	"..",
	"../../victim",
	`..\..\victim\..\..\..\..\victim`,
	`C:\victim`,
	"c:victim",
	"/abs",
	`\\server\share`,
	"a/b",
	".hidden",
	"CON",
	"nul.txt",
	"1.0.",
	"",
}

// rewriteBundle copies the bundle archive src to dst, replacing the JSON
// string old with new in the bundle index and the OCI layout indexes (where
// the tags are).
func rewriteBundle(t *testing.T, src, dst, old, new string) {
	t.Helper()
	in, err := os.Open(src)
	must.NoError(t, err)
	defer in.Close()
	var out bytes.Buffer
	tr, tw := tar.NewReader(in), tar.NewWriter(&out)
	quotedOld, _ := json.Marshal(old)
	quotedNew, _ := json.Marshal(new)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		must.NoError(t, err)
		data, err := io.ReadAll(tr)
		must.NoError(t, err)
		if hdr.Name == IndexFileName || strings.HasSuffix(hdr.Name, "/index.json") {
			data = bytes.ReplaceAll(data, quotedOld, quotedNew)
		}
		hdr.Size = int64(len(data))
		must.NoError(t, tw.WriteHeader(hdr))
		_, err = tw.Write(data)
		must.NoError(t, err)
	}
	must.NoError(t, tw.Close())
	must.NoError(t, os.WriteFile(dst, out.Bytes(), 0o644))
}

// TestBundle_CraftedTags checks that a bundle whose tags would escape the
// pack cache is rejected before anything is written (PK-1): when it is
// opened, and again by the cache install if an index got past that check.
func TestBundle_CraftedTags(t *testing.T) {
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
	_, err = b.AddArtifact(ctx, a, "9.9.9", "dir:simple_raw_exec", nil)
	must.NoError(t, err)
	good := filepath.Join(t.TempDir(), "good.tar")
	must.NoError(t, b.WriteFile(good))

	for _, tag := range craftedTags {
		t.Run(tag, func(t *testing.T) {
			// The builder refuses the tag.
			_, err := b.AddArtifact(ctx, a, tag, "dir:simple_raw_exec", nil)
			must.ErrorContains(t, err, "invalid tag")

			// A crafted bundle is refused when opened.
			evil := filepath.Join(t.TempDir(), "evil.tar")
			rewriteBundle(t, good, evil, "9.9.9", tag)
			_, err = Open(evil)
			must.ErrorContains(t, err, "invalid tag")

			// Defense in depth: the cache install checks the tag itself.
			bundle, err := Open(good)
			must.NoError(t, err)
			defer bundle.Close()
			bundle.Index.Packs[0].Tag = tag
			base := t.TempDir()
			cache := filepath.Join(base, "cache")
			must.NoError(t, os.Mkdir(cache, 0o755))
			_, err = bundle.ImportToCache(ctx, cache, "offline", packconfig.Policy{})
			must.ErrorContains(t, err, "invalid tag")
			entries, err := os.ReadDir(base)
			must.NoError(t, err)
			must.Len(t, 1, entries, must.Sprint("nothing is written next to the cache"))
			entries, err = os.ReadDir(cache)
			must.NoError(t, err)
			must.Len(t, 0, entries, must.Sprint("nothing is written into the cache"))
		})
	}

	// Crafted names and cache registry names are refused as well.
	for _, name := range []string{"..", `a\b`, "c:x", "CON", "aux.json", "x."} {
		_, err := Open(func() string {
			evil := filepath.Join(t.TempDir(), "evil.tar")
			rewriteBundle(t, good, evil, p.Metadata.Pack.Name, name)
			return evil
		}())
		must.ErrorContains(t, err, "invalid", must.Sprint(name))
	}
	bundle, err := Open(good)
	must.NoError(t, err)
	defer bundle.Close()
	for _, registry := range []string{"..", "../x", `..\x`, "C:", "NUL"} {
		_, err := bundle.ImportToCache(ctx, t.TempDir(), registry, packconfig.Policy{})
		must.ErrorContains(t, err, "invalid cache registry name", must.Sprint(registry))
	}
}
