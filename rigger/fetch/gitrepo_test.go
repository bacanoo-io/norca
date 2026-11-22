// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/bacanoo-io/norca/rigger/source"
)

// testRepo makes a repository with a module in modules/web, its version
// 1.0.0 tagged lightweight and 1.1.0 annotated, and a symlink that is not
// part of a module.
func testRepo(t *testing.T) (dir, first, second string) {
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

	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(1700000000, 0)}

	commit := func(version string) string {
		t.Helper()

		write(t, filepath.Join(dir, "modules", "web", "metadata.hcl"), "module \"web\" {\n  version = \""+version+"\"\n}\n")

		if _, err := wt.Add("modules"); err != nil {
			t.Fatal(err)
		}

		h, err := wt.Commit("web "+version, &git.CommitOptions{Author: sig})
		if err != nil {
			t.Fatal(err)
		}

		return h.String()
	}

	first = commit("1.0.0")

	if _, err := r.CreateTag("web/v1.0.0", mustHash(t, r), nil); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("metadata.hcl", filepath.Join(dir, "modules", "web", "link.hcl")); err != nil {
		t.Logf("no symlink on this system: %v", err)
	}

	second = commit("1.1.0")

	if _, err := r.CreateTag("web/v1.1.0", mustHash(t, r), &git.CreateTagOptions{Tagger: sig, Message: "web 1.1.0"}); err != nil {
		t.Fatal(err)
	}

	return dir, first, second
}

func mustHash(t *testing.T, r *git.Repository) [20]byte {
	t.Helper()

	ref, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}

	return ref.Hash()
}

func write(t *testing.T, path, data string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkVersions fails the test unless the versions of req are 1.0.0 at
// first and 1.1.0 (an annotated tag) at second.
func checkVersions(t *testing.T, f *Fetcher, req source.Request, first, second string) {
	t.Helper()

	versions, err := f.Versions(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, v := range versions {
		got[v.Version] = v.Commit
	}

	if len(got) != 2 || got["1.0.0"] != first || got["1.1.0"] != second {
		t.Fatalf("versions %+v, want 1.0.0 at %s and 1.1.0 (annotated) at %s", versions, first, second)
	}
}

// A repository on the local file system is read in place with go-git: its
// lightweight and annotated tags are versions with their commits, and the
// tree of a version holds its regular files.
func TestGitLocalRepository(t *testing.T) {
	t.Parallel()

	repo, first, second := testRepo(t)
	f := New(t.TempDir())
	req := source.Request{Kind: source.KindGit, Source: repo, Repository: repo, Path: "modules/web", TagPrefix: "web/v"}

	checkVersions(t, f, req, first, second)

	req.Version = "1.1.0"

	fetched, err := f.Fetch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if fetched.Origin.Commit != second || fetched.Origin.Ref != "web/v1.1.0" {
		t.Errorf("origin %+v", fetched.Origin)
	}

	data, err := fs.ReadFile(fetched.FS, fetched.Dir+"/metadata.hcl")
	if err != nil || string(data) != "module \"web\" {\n  version = \"1.1.0\"\n}\n" {
		t.Errorf("metadata.hcl %q, %v", data, err)
	}

	if _, err := fs.Stat(fetched.FS, fetched.Dir+"/link.hcl"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a symlink was written: %v", err)
	}

	// A lock file that records another commit for the tag is refused.
	req.Commit = first
	if _, err := f.Fetch(context.Background(), req); !errors.Is(err, ErrTagMoved) {
		t.Errorf("err %v, want ErrTagMoved", err)
	}

	// A tag that does not exist.
	req.Version, req.Commit = "9.9.9", ""
	if _, err := f.Fetch(context.Background(), req); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err %v, want fs.ErrNotExist", err)
	}

	// Tags lists every tag.
	tags, err := f.Tags(context.Background(), "file://"+filepath.ToSlash(repo))
	if err != nil || len(tags) != 2 {
		t.Errorf("tags %+v, %v", tags, err)
	}
}

// The credentials of the host become the authentication of go-git: a
// password (a token) over https, nothing without credentials.
func TestGitCredentials(t *testing.T) {
	t.Parallel()

	f := New(t.TempDir())

	auth, err := f.auth(context.Background(), "example.com/r", "https://example.com/r")
	if err != nil || auth != nil {
		t.Fatalf("without credentials: %v, %v", auth, err)
	}

	f.SetCredentials(func(_ context.Context, repository string) (*Credentials, error) {
		if repository != "example.com/r" {
			return nil, nil
		}

		return &Credentials{Password: "token"}, nil
	})

	auth, err = f.auth(context.Background(), "example.com/r", "https://example.com/r")
	if err != nil {
		t.Fatal(err)
	}

	basic, ok := auth.(*originAuth)
	if !ok || basic.Username != "git" || basic.Password != "token" {
		t.Errorf("auth %#v, want basic auth git/token", auth)
	}

	if auth, _ := f.auth(context.Background(), "example.com/other", "https://example.com/other"); auth != nil {
		t.Errorf("another repository got %#v", auth)
	}

	// An ssh key that is not a key is an error naming the repository.
	f.SetCredentials(func(context.Context, string) (*Credentials, error) {
		return &Credentials{SSHKey: []byte("not a key")}, nil
	})

	if _, err := f.auth(context.Background(), "ssh://example.com/r", "ssh://example.com/r"); err == nil {
		t.Error("an invalid ssh key was accepted")
	}
}
