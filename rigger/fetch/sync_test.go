// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/bacanoo-io/norca/rigger/source"
)

// A repository on the local file system: Sync gives the commit of its
// default branch; Checkout and TagsAt read it at a commit, the versions of
// that commit and none made after it.
func TestSync_LocalRepository(t *testing.T) {
	t.Parallel()

	dir, first, second := testRepo(t)
	f := New(t.TempDir())
	ctx := context.Background()

	commit, err := f.Sync(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	if commit != second {
		t.Errorf("Sync() = %s, want the head %s", commit, second)
	}

	checkTagsAt(t, f, dir, first, "web/v1.0.0")
	checkTagsAt(t, f, dir, second, "web/v1.0.0 web/v1.1.0")

	tree, err := f.Checkout(ctx, dir, first)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(tree, "modules", "web", "metadata.hcl"))
	if err != nil || !strings.Contains(string(data), `"1.0.0"`) {
		t.Errorf("the tree of the first commit has %q (%v), want version 1.0.0", data, err)
	}

	missing := strings.Repeat("0", 40)
	if _, err := f.Checkout(ctx, dir, missing); !errors.Is(err, ErrNoCommit) {
		t.Errorf("Checkout() of a missing commit = %v, want ErrNoCommit", err)
	}

	if _, err := f.Checkout(ctx, dir, "main"); err == nil {
		t.Error("a ref that is not a commit was checked out")
	}
}

// A commit is named by its full hash and nothing else. go-git keeps the
// first forty digits of a longer text: the commit they name is not the
// commit of the text, and its tree is not extracted once more under each
// such name.
func TestFetcher_CommitIsAFullHash(t *testing.T) {
	t.Parallel()

	dir, first, _ := testRepo(t)

	tests := []struct {
		name   string
		commit string
	}{
		{name: "the length of SHA-256", commit: first + strings.Repeat("0", 24)},
		{name: "a digit more", commit: first + "0"},
		{name: "some digits more", commit: first + "abcdef"},
		{name: "a digit less", commit: first[:39]},
		{name: "capitals", commit: strings.ToUpper(first)},
		{name: "no hex", commit: first[:38] + "zz"},
		{name: "a branch", commit: "main"},
		{name: "empty", commit: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := New(t.TempDir())

			for name, repository := range map[string]string{"a directory": dir, "a URL": noNetwork} {
				tree, err := f.Checkout(t.Context(), repository, tt.commit)
				if !errors.Is(err, errCommit) {
					t.Errorf("Checkout() of %s = %q, %v; want errCommit", name, tree, err)
				}

				tags, err := f.TagsAt(t.Context(), repository, tt.commit, "web/v")
				if !errors.Is(err, errCommit) {
					t.Errorf("TagsAt() of %s = %v, %v; want errCommit", name, tags, err)
				}
			}

			if entries, _ := os.ReadDir(filepath.Join(f.cache, "trees")); len(entries) > 0 {
				t.Errorf("%d trees were extracted", len(entries))
			}
		})
	}

	// The cache serves what a lock file pins by a full hash only, SHA-1 or
	// SHA-256 (the commit of a bundle).
	f := New(t.TempDir())

	for commit, served := range map[string]bool{
		strings.Repeat("a", 40): true, strings.Repeat("a", 64): true, strings.Repeat("a", 41): false, strings.Repeat("a", 63): false,
	} {
		write(t, filepath.Join(f.TreeDir(noNetwork, commit), testDir, "metadata.hcl"), "module {}\n")

		if _, ok := f.cachedTree(source.Request{Repository: noNetwork, Commit: commit, Path: testDir}); ok != served {
			t.Errorf("cachedTree() of a commit of %d digits = %v, want %v", len(commit), ok, served)
		}
	}
}

// A repository on a URL is read from its mirror as it is: a commit the
// mirror has needs no fetch; one it lacks makes it fetch once.
func TestSync_Mirror(t *testing.T) {
	t.Parallel()

	src, first, second := testRepo(t)
	f := New(t.TempDir())
	ctx := context.Background()

	// The address of the registry; its mirror fetches from its origin, the
	// repository src, as a mirror made by an earlier sync does.
	const remote = "registry.invalid/acme/modules"

	if _, err := f.updateMirror(ctx, src, f.mirrorDir(remote), nil); err != nil {
		t.Fatal(err)
	}

	checkTagsAt(t, f, remote, first, "web/v1.0.0")

	if _, err := f.Checkout(ctx, remote, second); err != nil {
		t.Fatalf("a commit of the mirror: %v", err)
	}

	// A commit made after the mirror: Checkout fetches it once; TagsAt at
	// the earlier commit does not see the tag made on it.
	third := commitTo(t, src, "1.2.0", "web/v1.2.0")

	if _, err := f.Checkout(ctx, remote, third); err != nil {
		t.Fatalf("a commit made after the mirror: %v", err)
	}

	checkTagsAt(t, f, remote, second, "web/v1.0.0 web/v1.1.0")
	checkTagsAt(t, f, remote, third, "web/v1.0.0 web/v1.1.0 web/v1.2.0")

	// A commit no one has.
	if _, err := f.Checkout(ctx, remote, strings.Repeat("1", 40)); !errors.Is(err, ErrNoCommit) {
		t.Errorf("Checkout() of a commit no one has = %v, want ErrNoCommit", err)
	}

	// Sync fetches again and gives the head.
	fourth := commitTo(t, src, "1.3.0", "")

	commit, err := f.Sync(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}

	if commit != fourth {
		t.Errorf("Sync() = %s, want the new head %s", commit, fourth)
	}

	// A version tagged after the mirror was fetched in this run is fetched
	// once more by its tag, as a server installs it.
	fifth := commitTo(t, src, "1.4.0", "web/v1.4.0")

	_, got, err := f.Tree(ctx, remote, "web/v1.4.0")
	if err != nil {
		t.Fatalf("a version tagged after the mirror was fetched: %v", err)
	}

	if got != fifth {
		t.Errorf("Tree() commit = %s, want %s", got, fifth)
	}

	if _, _, err := f.Tree(ctx, remote, "web/v9.9.9"); err == nil {
		t.Error("a tag no one has was found")
	}
}

// commitTo commits a new version of modules/web to the repository in dir
// and tags it when tag is not empty; it returns the commit.
func commitTo(t *testing.T, dir, version, tag string) string {
	t.Helper()

	r, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(dir, "modules", "web", "metadata.hcl"), "module \"web\" {\n  version = \""+version+"\"\n}\n")

	if _, err := wt.Add("modules"); err != nil {
		t.Fatal(err)
	}

	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(1700000100, 0)}

	h, err := wt.Commit("web "+version, &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}

	if tag != "" {
		if _, err := r.CreateTag(tag, h, nil); err != nil {
			t.Fatal(err)
		}
	}

	return h.String()
}

func checkTagsAt(t *testing.T, f *Fetcher, repository, commit, want string) {
	t.Helper()

	tags, err := f.TagsAt(context.Background(), repository, commit, "web/v")
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		names = append(names, tag.Name)
	}

	if got := strings.Join(names, " "); got != want {
		t.Errorf("TagsAt(%s) = %q, want %q", commit[:8], got, want)
	}
}
