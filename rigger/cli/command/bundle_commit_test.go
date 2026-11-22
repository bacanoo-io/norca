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

// A registry of many modules tags them at one commit: the bundle of the
// second does not lose against that of the first.
func TestImportBundle_TwoModulesOfACommit(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("RIGGER_CACHE_DIR", cache)

	eng, err := rigger.New()
	if err != nil {
		t.Fatal(err)
	}

	const repo = "example.com/acme/registry"

	commit, f := strings.Repeat("c", 40), fetch.New(cache)

	for _, name := range []string{"nats", "postgres"} {
		dir := "modules/" + name
		root := fstest.MapFS{dir + "/metadata.hcl": {Data: []byte("module {\n  name    = \"" + name + "\"\n  version = \"1.0.0\"\n}\n")}}

		digest, err := eng.ModuleDigest(t.Context(), root, dir)
		if err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		if err := bundle.Write(&buf, name, []rigger.SourcedModule{{
			Source: repo + "//" + dir, Version: bundledVersion, Repository: repo, Path: dir, Ref: name + "/v1.0.0",
			Commit: commit, Digest: digest, Dirs: []string{dir}, Root: root,
		}}); err != nil {
			t.Fatal(err)
		}

		file := filepath.Join(t.TempDir(), name+".bundle")
		if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := importBundle(t.Context(), slog.New(slog.DiscardHandler), io.Discard, file); err != nil {
			t.Fatal(err)
		}

		placed := filepath.Join(f.BundleDir(repo, commit, dir, digest), filepath.FromSlash(dir), "metadata.hcl")
		if _, err := os.Stat(placed); err != nil {
			t.Errorf("the import of %s said it worked and it is not there: %v", name, err)
		}
	}
}
