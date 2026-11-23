// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/bacanoo-io/norca/rigger/source"
)

// A tag is named, not a path to another reference: a branch moves.
func TestTagCommit_OnlyTags(t *testing.T) {
	t.Parallel()

	r, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := wt.Commit("first", &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}

	name := "../heads/" + head.Name().Short()

	if got, err := tagCommit(r, name); err == nil {
		t.Errorf("tagCommit(%q) = %s, a branch was taken as a tag", name, got)
	}
}

// tagFile tags a file of a commit of the repository in dir: a tag of what
// is no commit.
func tagFile(t *testing.T, dir, commit, name, tag string) {
	t.Helper()

	r, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}

	c, err := r.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		t.Fatal(err)
	}

	file, err := c.File(name)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.CreateTag(tag, file.Hash, nil); err != nil {
		t.Fatal(err)
	}
}

// "No such tag" is the error of a tag the repository does not have, and of
// nothing else: a tag that can't be read, and a fetch for it that failed,
// are the errors they are.
func TestFetcher_TagErrors(t *testing.T) {
	t.Parallel()

	t.Run("no such tag", func(t *testing.T) {
		t.Parallel()

		repo, _, _ := testRepo(t)

		_, err := New(t.TempDir()).Fetch(t.Context(), source.Request{Kind: source.KindGit, Repository: repo, Path: webPath, Ref: "web/v9.9.9"})
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Fetch() of a tag no one made = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("a tag of no commit", func(t *testing.T) {
		t.Parallel()

		repo, first, _ := testRepo(t)
		tagFile(t, repo, first, webPath+"/metadata.hcl", "blob/v1.0.0")

		_, err := New(t.TempDir()).Fetch(t.Context(), source.Request{Kind: source.KindGit, Repository: repo, Path: webPath, Ref: "blob/v1.0.0"})
		if err == nil || errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), "has no tag") {
			t.Errorf("Fetch() of a tag of a file = %v, want the error of the tag, not that there is none", err)
		}
	})

	t.Run("a fetch that failed", func(t *testing.T) {
		t.Parallel()

		src, _, _ := testRepo(t)
		f := New(t.TempDir())

		// The mirror of a repository at a URL, fetched in this run; its
		// origin is gone when the tag is looked for once more.
		const remote = "registry.invalid/acme/modules"

		mirror := f.mirrorDir(remote)
		if _, err := f.updateMirror(t.Context(), src, mirror, nil); err != nil {
			t.Fatal(err)
		}

		f.setUpdated(mirror)

		if err := os.Rename(src, src+".gone"); err != nil {
			t.Fatal(err)
		}

		_, err := f.Fetch(t.Context(), source.Request{Kind: source.KindGit, Repository: remote, Path: webPath, Ref: "web/v9.9.9"})
		if err == nil || errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), "has no tag") {
			t.Errorf("Fetch() of a tag whose fetch failed = %v, want the error of the fetch", err)
		}
	})
}
