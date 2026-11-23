// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"

	"github.com/bacanoo-io/norca/rigger/source"
)

// repoOf makes a repository with one commit of the files given, tagged
// v1.0.0, and returns its directory and the commit.
func repoOf(t *testing.T, files map[string][]byte) (dir, commit string) {
	t.Helper()

	dir = t.TempDir()

	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	for name, data := range files {
		write(t, filepath.Join(dir, filepath.FromSlash(name)), string(data))
	}

	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}

	h, err := wt.Commit("files", &git.CommitOptions{Author: testAuthor()})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.CreateTag(testRef, h, nil); err != nil {
		t.Fatal(err)
	}

	return dir, h.String()
}

// The tree of a commit is small in a repository and may be large on the
// disk (a blob of zeros, a million files): what the fetcher extracts of one
// commit has a bound per file, per tree and of the number of files, and a
// tree past one of them leaves nothing in the cache.
func TestFetcher_SetLimits(t *testing.T) {
	t.Parallel()

	const kib = 1 << 10

	const metadataFile = "metadata.hcl"

	metadata := []byte("module {}\n")

	tests := []struct {
		name    string
		files   map[string][]byte
		limits  Limits
		refused bool
	}{
		{
			name:    "a file past its bound",
			files:   map[string][]byte{metadataFile: metadata, "blob": make([]byte, 64*kib)},
			limits:  Limits{FileBytes: 16 * kib},
			refused: true,
		},
		{
			name:   "a file at its bound",
			files:  map[string][]byte{metadataFile: metadata, "blob": make([]byte, 16*kib)},
			limits: Limits{FileBytes: 16 * kib},
		},
		{
			name: "files past the bound of the tree",
			files: map[string][]byte{
				metadataFile: metadata, "a": bytes.Repeat([]byte("a"), 8*kib), "b/b": bytes.Repeat([]byte("b"), 8*kib),
				"c/c/c": bytes.Repeat([]byte("c"), 8*kib),
			},
			limits:  Limits{TreeBytes: 20 * kib},
			refused: true,
		},
		{
			name: "files within the bound of the tree",
			files: map[string][]byte{
				metadataFile: metadata, "a": bytes.Repeat([]byte("a"), 8*kib), "b/b": bytes.Repeat([]byte("b"), 8*kib),
			},
			limits: Limits{TreeBytes: 20 * kib},
		},
		{
			name:    "more files than the bound",
			files:   map[string][]byte{metadataFile: metadata, "a": []byte("a"), "b": []byte("b"), "c": []byte("c")},
			limits:  Limits{TreeFiles: 3},
			refused: true,
		},
		{
			name:   "as many files as the bound",
			files:  map[string][]byte{metadataFile: metadata, "a": []byte("a"), "b": []byte("b")},
			limits: Limits{TreeFiles: 3},
		},
		{
			name:  "the default bounds",
			files: map[string][]byte{metadataFile: metadata, "blob": make([]byte, 64*kib)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, commit := repoOf(t, tt.files)

			f := New(t.TempDir())
			f.SetLimits(tt.limits)

			calls := map[string]func(*Fetcher) error{
				"Checkout": func(f *Fetcher) error {
					_, err := f.Checkout(t.Context(), repo, commit)

					return err
				},
				"Fetch": func(f *Fetcher) error {
					_, err := f.Fetch(t.Context(), source.Request{Kind: source.KindGit, Repository: repo, Path: ".", Ref: testRef})

					return err
				},
				"a request of the host": func(f *Fetcher) error {
					_, err := f.WithGlobalRegistry(t.TempDir()).Checkout(t.Context(), repo, commit)

					return err
				},
			}

			for name, call := range calls {
				err := call(f)

				if got := errors.Is(err, ErrTreeTooLarge); got != tt.refused {
					t.Errorf("%s: refused for its size = %v, want %v: %v", name, got, tt.refused, err)
				}

				if !tt.refused && err != nil {
					t.Errorf("%s = %v", name, err)
				}

				if !tt.refused {
					continue
				}

				// Nothing of the tree stays: not the tree, not what was
				// written of it before the bound.
				entries, err := os.ReadDir(filepath.Dir(f.TreeDir(repo, commit)))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}

				if len(entries) > 0 {
					t.Errorf("%s: the cache keeps %d entries of a tree past its bound, %s first", name, len(entries), entries[0].Name())
				}
			}
		})
	}
}

// The default bounds are positive, and a bound that is not set keeps its
// default.
func TestLimits_Defaults(t *testing.T) {
	t.Parallel()

	d := DefaultLimits()
	if d.TreeBytes <= 0 || d.FileBytes <= 0 || d.TreeFiles <= 0 || d.FileBytes > d.TreeBytes {
		t.Errorf("DefaultLimits() = %+v", d)
	}

	f := New(t.TempDir())
	f.SetLimits(Limits{FileBytes: 1, TreeFiles: -1})

	if want := (Limits{TreeBytes: d.TreeBytes, FileBytes: 1, TreeFiles: d.TreeFiles}); f.limits != want {
		t.Errorf("SetLimits() left %+v, want %+v", f.limits, want)
	}
}

// What a blob declares as its size is not trusted: a stream that unpacks to
// more is stopped at the bound of a file, and at what is left of the tree.
func TestTreeBudget_Reader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// written is what earlier files of the tree took.
		written  int64
		content  string
		expected error
	}{
		{name: "within both bounds", content: "12345678"},
		{name: "past the bound of a file", content: "123456789", expected: ErrTreeTooLarge},
		{name: "within what is left of the tree", written: 6, content: "1234"},
		{name: "past what is left of the tree", written: 6, content: "12345", expected: ErrTreeTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := &treeBudget{limits: Limits{TreeBytes: 10, FileBytes: 8, TreeFiles: 2}, bytes: tt.written}

			// The blob declares one byte.
			if err := b.file("blob", 1); err != nil {
				t.Fatal(err)
			}

			n, err := io.Copy(io.Discard, b.reader("blob", strings.NewReader(tt.content)))
			if !errors.Is(err, tt.expected) {
				t.Errorf("reading %d bytes after %d = %v, want %v", len(tt.content), tt.written, err, tt.expected)
			}

			if tt.expected == nil && (n != int64(len(tt.content)) || b.bytes != tt.written+n) {
				t.Errorf("read %d bytes, counted %d; want %d and %d", n, b.bytes, len(tt.content), tt.written+int64(len(tt.content)))
			}
		})
	}
}
