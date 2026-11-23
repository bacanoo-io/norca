// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

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

// testAuthor is the author of the commits of the tests.
func testAuthor() *object.Signature {
	return &object.Signature{Name: "author", Email: "author@example.com", When: time.Unix(1700000000, 0)}
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

// The path of the URL of a file is the path as the system writes it. On
// Windows the drive follows a slash of the URL (file:///C:/repos/x), which
// is no part of the path; elsewhere such a path is one below the root.
func TestFileURLPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		windows  bool
		expected string
	}{
		{name: "a drive", path: "/C:/repos/x", windows: true, expected: "C:/repos/x"},
		{name: "a drive in lower case", path: "/c:/repos/x", windows: true, expected: "c:/repos/x"},
		{name: "a drive alone", path: "/C:", windows: true, expected: "C:"},
		{name: "a drive without the slash of the URL", path: "C:/repos/x", windows: true, expected: "C:/repos/x"},
		{name: "no drive", path: "/repos/x", windows: true, expected: "/repos/x"},
		{name: "a directory that ends in a colon", path: "/CC:/x", windows: true, expected: "/CC:/x"},
		{name: "a file that starts like a drive", path: "/C:x", windows: true, expected: "/C:x"},
		{name: "empty", path: "", windows: true, expected: ""},
		{name: "a path like a drive, not on windows", path: "/C:/repos/x", expected: "/C:/repos/x"},
		{name: "a path of unix", path: "/srv/repos/x", expected: "/srv/repos/x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := fileURLPath(tt.path, tt.windows); got != tt.expected {
				t.Errorf("fileURLPath(%q, %v) = %q, want %q", tt.path, tt.windows, got, tt.expected)
			}
		})
	}
}

// A repository on the local file system is the same one by its path, by
// its URL (file:///C:/repos/x on Windows) and by the URL without the slash
// before a drive.
func TestLocalPath_Addresses(t *testing.T) {
	t.Parallel()

	repo, _, second := testRepo(t)
	slash := filepath.ToSlash(repo)

	addresses := map[string]string{
		"a path":           repo,
		"a URL":            "file:///" + strings.TrimPrefix(slash, "/"),
		"a URL of a drive": "file://" + slash,
	}

	f := New(t.TempDir())

	for name, address := range addresses {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if !LocalRepository(address) {
				t.Errorf("LocalRepository(%q) = false", address)
			}

			commit, err := f.Sync(t.Context(), address)
			if err != nil || commit != second {
				t.Errorf("Sync(%q) = %q, %v; want the head %s", address, commit, err, second)
			}

			if got, want := CanonicalRepository(address), CanonicalRepository(repo); got != want {
				t.Errorf("CanonicalRepository(%q) = %q, want that of the path, %q", address, got, want)
			}
		})
	}
}

// Two runs of the program share a cache and know nothing of each other's
// locks. A clone that fails leaves alone what another run has at the place
// of the mirror, and clones of one repository at the same time end with one
// mirror that is whole.
func TestUpdateMirror_OtherRuns(t *testing.T) {
	t.Parallel()

	t.Run("a clone that fails", func(t *testing.T) {
		t.Parallel()

		f := New(t.TempDir())
		mirror := f.mirrorDir(noNetwork)

		// What another run is writing there: not yet a repository.
		theirs := filepath.Join(mirror, "objects", "pack", "tmp_pack_of_another_run")
		write(t, theirs, "pack")

		if _, err := f.updateMirror(t.Context(), noNetwork, mirror, nil); err == nil {
			t.Fatal("a clone of a repository no one serves = nil")
		}

		if _, err := os.Stat(theirs); err != nil {
			t.Errorf("the failed clone removed what another run had at the mirror: %v", err)
		}

		entries, err := os.ReadDir(filepath.Dir(mirror))
		if err != nil || len(entries) != 1 {
			t.Errorf("the failed clone left %d entries beside the mirror, %v", len(entries), err)
		}
	})

	t.Run("clones at the same time", func(t *testing.T) {
		t.Parallel()

		src, _, second := testRepo(t)
		cache := t.TempDir()

		const runs = 8

		errs := make(chan error, runs)
		mirror := New(cache).mirrorDir(src)

		for range runs {
			go func() {
				// A fetcher of its own is a run of its own: no lock is shared.
				_, err := New(cache).updateMirror(t.Context(), src, mirror, nil)
				errs <- err
			}()
		}

		for range runs {
			if err := <-errs; err != nil {
				t.Errorf("a clone beside another = %v", err)
			}
		}

		r, err := git.PlainOpen(mirror)
		if err != nil {
			t.Fatalf("the mirror after the clones: %v", err)
		}

		if commit, err := headCommit(r); err != nil || commit != second {
			t.Errorf("the mirror after the clones has the head %s, %v; want %s", commit, err, second)
		}

		entries, err := os.ReadDir(filepath.Dir(mirror))
		if err != nil || len(entries) != 1 {
			t.Errorf("the clones left %d entries in the cache beside the mirror, %v", len(entries), err)
		}
	})
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
	if !ok || basic.Username != gitUser || basic.Password != "token" {
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

// commitInMemory makes a repository in memory with one commit of the files
// given (names without a directory), and returns it and the commit: a tree
// that a worktree of this system might not hold.
func commitInMemory(t *testing.T, files map[string]string) (*git.Repository, string) {
	t.Helper()

	r, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}

	store := func(kind plumbing.ObjectType, encode func(plumbing.EncodedObject) error) plumbing.Hash {
		obj := r.Storer.NewEncodedObject()
		obj.SetType(kind)

		if err := encode(obj); err != nil {
			t.Fatal(err)
		}

		h, err := r.Storer.SetEncodedObject(obj)
		if err != nil {
			t.Fatal(err)
		}

		return h
	}

	tree := &object.Tree{}

	for _, name := range slices.Sorted(maps.Keys(files)) {
		blob := store(plumbing.BlobObject, func(obj plumbing.EncodedObject) error {
			w, err := obj.Writer()
			if err != nil {
				return err
			}

			if _, err := w.Write([]byte(files[name])); err != nil {
				return err
			}

			return w.Close()
		})

		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blob})
	}

	commit := &object.Commit{Author: *testAuthor(), Committer: *testAuthor(), Message: "files", TreeHash: store(plumbing.TreeObject, tree.Encode)}

	return r, store(plumbing.CommitObject, commit.Encode).String()
}

// Two files whose names differ only by case are two files in git and one
// name on the file systems of Windows and macOS. Where they are two, both
// are written; where they are one, the error says so and does not read as
// a cache that is broken.
func TestWriteTree_NamesByCase(t *testing.T) {
	t.Parallel()

	r, commit := commitInMemory(t, map[string]string{"README": "upper\n", "readme": "lower\n", "metadata.hcl": "module {}\n"})

	probe := t.TempDir()
	write(t, filepath.Join(probe, "Probe"), "")

	_, statErr := os.Stat(filepath.Join(probe, "probe"))
	byCase := statErr != nil

	dir := t.TempDir()
	err := writeTree(r, commit, dir, DefaultLimits())

	if byCase {
		upper, _ := os.ReadFile(filepath.Join(dir, "README"))
		lower, _ := os.ReadFile(filepath.Join(dir, "readme"))

		if err != nil || string(upper) != "upper\n" || string(lower) != "lower\n" {
			t.Errorf("writeTree() = %v, README %q, readme %q; want both files", err, upper, lower)
		}

		return
	}

	if !errors.Is(err, errSameName) || !strings.Contains(err.Error(), "readme") {
		t.Errorf("writeTree() on a file system that does not tell names by case = %v, want errSameName of readme", err)
	}
}
