// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

// Two repositories of one commit (a fork) are both in the cache after the
// import of their bundle: each is read by its own.
func TestImportBundle_TwoRepositories(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("RIGGER_CACHE_DIR", cache)

	root := fstest.MapFS{"mod/metadata.hcl": {Data: []byte("module {\n  name    = \"mod\"\n  version = \"1.0.0\"\n}\n")}}

	eng, err := rigger.New()
	if err != nil {
		t.Fatal(err)
	}

	digest, err := eng.ModuleDigest(t.Context(), root, "mod")
	if err != nil {
		t.Fatal(err)
	}

	commit := strings.Repeat("a", 40)
	repositories := []string{"example.com/acme/modules", "example.com/fork/modules"}

	mods := make([]rigger.SourcedModule, 0, len(repositories))
	for _, repo := range repositories {
		mods = append(mods, rigger.SourcedModule{
			Source: repo + "//mod", Version: bundledVersion, Repository: repo, Path: "mod", Ref: "mod/v1.0.0",
			Commit: commit, Digest: digest, Dirs: []string{"mod"}, Root: root,
		})
	}

	var buf bytes.Buffer
	if err := bundle.Write(&buf, "fork", mods); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "fork.bundle")
	if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := importBundle(t.Context(), slog.New(slog.DiscardHandler), io.Discard, file); err != nil {
		t.Fatal(err)
	}

	f := fetch.New(cache)

	dirs := map[string]bool{}

	for _, repo := range repositories {
		dir := f.BundleDir(repo, commit, "mod", digest)
		dirs[dir] = true

		if _, err := os.Stat(filepath.Join(dir, "mod", "metadata.hcl")); err != nil {
			t.Errorf("%s is not in the cache: %v", repo, err)
		}
	}

	if len(dirs) != len(repositories) {
		t.Error("the two repositories share one directory of the cache")
	}
}
