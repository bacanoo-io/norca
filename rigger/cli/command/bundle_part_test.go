// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
	"github.com/bacanoo-io/norca/rigger/source"
)

// partSource serves one published stack.
type partSource struct{ root fstest.MapFS }

func (p partSource) Versions(_ context.Context, req source.Request) ([]source.Available, error) {
	return []source.Available{{Version: bundledVersion, Ref: req.TagPrefix + bundledVersion, Commit: strings.Repeat("e", 40)}}, nil
}

func (p partSource) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	return source.Fetched{
		FS: p.root, Dir: req.Path,
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: req.TagPrefix + req.Version, Commit: strings.Repeat("e", 40)},
	}, nil
}

// A bundle of a stack with a part holds a published stack: an import
// checks it by the digest of a stack.
func TestImportBundle_Part(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	published := fstest.MapFS{
		"stacks/data/metadata.hcl":            {Data: []byte("stack {\n  name    = \"data\"\n  version = \"" + bundledVersion + "\"\n}\n")},
		"stacks/data/stack.hcl":               {Data: []byte("rigger {\n  name = \"data\"\n}\n\nmodule \"db\" {\n  source = \"./modules/db\"\n}\n")},
		"stacks/data/modules/db/metadata.hcl": {Data: []byte("module {\n  name    = \"db\"\n  version = \"" + bundledVersion + "\"\n}\n")},
	}
	local := fstest.MapFS{
		"s/stack.hcl": {Data: []byte("rigger {\n  name = \"site\"\n}\n\nstack \"data\" {\n  source = \"example.com/acme/stacks//stacks/data\"\n}\n")},
	}

	eng, err := rigger.New(rigger.WithFetcher(partSource{published}), rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	_, lock, diags := eng.SyncStack(t.Context(), local, "s", false)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	local["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

	st, diags := eng.LoadStack(t.Context(), local, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	var buf bytes.Buffer
	if err := bundle.Write(&buf, st.Name(), st.SourcedModules()); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "site.bundle")
	if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := importBundle(t.Context(), slog.New(slog.DiscardHandler), io.Discard, file); err != nil {
		t.Errorf("the import of a bundle with a part: %v", err)
	}
}
