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

// What a lock file records names paths in the cache: none of it leads out
// of the cache, and a source that can't be reached is where it is read.
func TestFetcher_LockPathsStayInCache(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	manifest := "sha256:" + strings.Repeat("b", 64)

	tests := []struct {
		name string
		req  source.Request
	}{
		{
			name: "oci manifest",
			req:  source.Request{Kind: source.KindOCI, Repository: "127.0.0.1:1/x", Version: testVersion, Commit: "../../outside", Path: testDir},
		},
		{
			name: "oci path",
			req:  source.Request{Kind: source.KindOCI, Repository: "127.0.0.1:1/x", Version: testVersion, Commit: manifest, Path: "../../../outside/mod"},
		},
		{
			name: "git path",
			req:  source.Request{Kind: source.KindGit, Repository: "http://127.0.0.1:1/x", Ref: testRef, Commit: commit, Path: "../../../outside/mod"},
		},
		{
			name: "git path of windows",
			req:  source.Request{Kind: source.KindGit, Repository: "http://127.0.0.1:1/x", Ref: testRef, Commit: commit, Path: `..\..\..\outside\mod`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			f := New(filepath.Join(root, "cache"))

			outside := filepath.Join(root, "outside", "mod")

			for _, dir := range []string{outside, f.OCIDir(tt.req.Repository, manifest), f.TreeDir(tt.req.Repository, commit)} {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatal(err)
				}
			}

			if err := os.WriteFile(filepath.Join(outside, "metadata.hcl"), []byte("module {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if got, err := f.Fetch(t.Context(), tt.req); err == nil {
				t.Errorf("Fetch() = dir %q; a path out of the cache was taken", got.Dir)
			}
		})
	}
}
