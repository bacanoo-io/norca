// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/source"
)

// indexDir is a checkout of a global registry that lists norca/nats at a
// version.
func indexDir(t *testing.T, version string) string {
	t.Helper()

	e := index.Entry{Namespace: "norca", Name: "nats", Versions: []index.Version{{
		Version: version, Repository: "github.com/norca/rigger", Path: "modules/nats", Tag: "nats/v" + version,
		Commit: strings.Repeat("a", 40), Digest: "sha256:" + strings.Repeat("b", 64),
		Signatures: []index.Signature{{KeyID: "sha256:" + strings.Repeat("c", 64), Signature: "MEUCIQ=="}}, Listed: "2026-11-22",
	}}}

	data, err := index.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, filepath.FromSlash(index.EntryPath(index.KindModule, e.Namespace, e.Name)))

	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}

	return dir
}

// A host that serves several requests from one cache gives each the index
// it read: a later request does not move the index of one that runs.
func TestFetcher_GlobalRegistryOfARequest(t *testing.T) {
	t.Parallel()

	host := New(t.TempDir())
	before, after := indexDir(t, "1.0.0"), indexDir(t, "2.0.0")

	first := host.WithGlobalRegistry(before)
	second := host.WithGlobalRegistry(after)

	req := source.Request{Source: "norca/nats", Kind: source.KindGlobal, Namespace: "norca", Name: "nats"}

	for _, tt := range []struct {
		fetcher  *Fetcher
		expected string
	}{{first, "1.0.0"}, {second, "2.0.0"}} {
		got, err := tt.fetcher.Versions(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}

		if len(got) != 1 || got[0].Version != tt.expected {
			t.Errorf("Versions() = %v, want %s", got, tt.expected)
		}
	}
}
