// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/source"
)

// What the tests of the cache name a module by.
const (
	testDir     = "mod"
	testVersion = "1.0.0"
	testRef     = "v1.0.0"
)

// What the cache keeps is its repository's: a lock that names another,
// unreachable one and a commit it knows reads none of it.
func TestFetcher_CacheOfARepository(t *testing.T) {
	t.Parallel()

	commit, manifest := strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64)

	bundled := "sha256:" + strings.Repeat("d", 64)

	const (
		gitRepo = "https://127.0.0.1:1/private"
		ociRepo = "127.0.0.1:1/private"
	)

	tests := []struct {
		name string
		req  source.Request
		dir  func(*Fetcher) string
	}{
		{
			name: "tree",
			req:  source.Request{Kind: source.KindGit, Repository: gitRepo, Ref: testRef, Commit: commit, Path: testDir},
			dir:  func(f *Fetcher) string { return f.TreeDir(gitRepo, commit) },
		},
		{
			name: "bundle",
			req:  source.Request{Kind: source.KindGit, Repository: gitRepo, Ref: testRef, Commit: commit, Path: testDir, Digest: bundled},
			dir:  func(f *Fetcher) string { return f.BundleDir(gitRepo, commit, testDir, bundled) },
		},
		{
			name: "layer",
			req:  source.Request{Kind: source.KindOCI, Repository: ociRepo, Version: testVersion, Commit: manifest, Path: testDir},
			dir:  func(f *Fetcher) string { return f.OCIDir(ociRepo, manifest) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := New(t.TempDir())
			dir := filepath.Join(tt.dir(f), testDir)

			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(dir, "metadata.hcl"), []byte("module {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			other := tt.req
			other.Repository = strings.Replace(tt.req.Repository, "private", "unrelated", 1)

			if _, err := f.Fetch(t.Context(), other); err == nil {
				t.Error("Fetch() of another repository read what the cache has of the private one")
			}

			if _, err := f.Fetch(t.Context(), tt.req); err != nil {
				t.Errorf("Fetch() of the repository itself without a network: %v", err)
			}
		})
	}
}
