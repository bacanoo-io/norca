// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"

	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/source"
)

// The module of the tests of the global registry: norca/web, in modules/web
// of the repository of testRepo.
const (
	webNamespace = "norca"
	webName      = "web"
	webPath      = "modules/web"
	webTag       = "web/v1.0.0"
)

// noNetwork is the address of a registry that can't be reached: nothing
// listens there, as on a machine without a network.
const noNetwork = "http://127.0.0.1:1/registry"

// blockedWeb is the advisories of a registry that blocks norca/web 1.0.0.
const blockedWeb = `{"advisories":[{"kind":"module","namespace":"norca","name":"web","version":"1.0.0","level":"blocked","reason":"malware","decision":"d-1"}]}`

// lockedWeb is the request of norca/web 1.0.0 as a lock file records it:
// the repository of its author and the commit of its tag.
func lockedWeb(repository, commit string) source.Request {
	return source.Request{
		Source: webNamespace + "/" + webName, Kind: source.KindGlobal, Namespace: webNamespace, Name: webName,
		Repository: repository, Path: webPath, Version: testVersion, Ref: webTag, Commit: commit,
	}
}

// registryFiles is the files of a global registry that lists norca/web
// 1.0.0 of a repository, with the advisories given.
func registryFiles(t *testing.T, repository, commit, advisories string) map[string]string {
	t.Helper()

	e := index.Entry{Namespace: webNamespace, Name: webName, Versions: []index.Version{{
		Version: testVersion, Repository: repository, Path: webPath, Tag: webTag,
		Commit: commit, Digest: "sha256:" + strings.Repeat("b", 64),
		Signatures: []index.Signature{{KeyID: "sha256:" + strings.Repeat("c", 64), Signature: "MEUCIQ=="}}, Listed: "2026-11-23",
	}}}

	data, err := index.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	return map[string]string{
		index.EntryPath(index.KindModule, e.Namespace, e.Name): string(data),
		index.AdvisoriesFile: advisories,
	}
}

// registryDir writes the files of a registry into a directory.
func registryDir(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, data := range files {
		write(t, filepath.Join(dir, filepath.FromSlash(name)), data)
	}

	return dir
}

// mirrorOf puts a mirror of a registry with the files given into the cache
// of f, as an earlier run made it of the registry at address.
func mirrorOf(t *testing.T, f *Fetcher, address string, files map[string]string) {
	t.Helper()

	src := registryDir(t, files)

	r, err := git.PlainInit(src, false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}

	if _, err := wt.Commit("index", &git.CommitOptions{Author: testAuthor()}); err != nil {
		t.Fatal(err)
	}

	mirror, err := f.updateMirror(t.Context(), src, f.mirrorDir(address), nil)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := mirror.Config()
	if err != nil {
		t.Fatal(err)
	}

	cfg.Remotes["origin"].URLs = []string{address}

	if err := mirror.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// A registry that was reached and can't be read or does not hold says
// nothing of a version, also not that it is blocked: the version a lock
// file records is not fetched past it (design 7.3 lets the lock entry serve
// only on a machine without a network).
func TestFetcher_GlobalFetch_IndexUnreadable(t *testing.T) {
	t.Parallel()

	ended, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		// registry returns the address of the global registry.
		registry func(t *testing.T, repository, commit string) string
		ctx      context.Context //nolint:containedctx // the context of a case
		expected error
	}{
		{
			name: "advisories with a field of a later format",
			registry: func(t *testing.T, repository, commit string) string {
				t.Helper()

				return registryDir(t, registryFiles(t, repository, commit, strings.Replace(blockedWeb, `"decision"`, `"added":"2026-11-23","decision"`, 1)))
			},
			expected: index.ErrInvalid,
		},
		{
			name: "advisories that are no JSON",
			registry: func(t *testing.T, repository, commit string) string {
				t.Helper()

				return registryDir(t, registryFiles(t, repository, commit, "<html>"))
			},
			expected: index.ErrInvalid,
		},
		{
			name: "advisories that break their format",
			registry: func(t *testing.T, repository, commit string) string {
				t.Helper()

				return registryDir(t, registryFiles(t, repository, commit, strings.Replace(blockedWeb, `"blocked"`, `"fatal"`, 1)))
			},
			expected: index.ErrInvalid,
		},
		{
			name: "a directory that is not there",
			registry: func(t *testing.T, _, _ string) string {
				t.Helper()

				return filepath.Join(t.TempDir(), "missing")
			},
			expected: git.ErrRepositoryNotExists,
		},
		{
			name:     "a context that ended",
			registry: func(*testing.T, string, string) string { return noNetwork },
			ctx:      ended,
			expected: context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, first, _ := testRepo(t)

			f := New(t.TempDir())
			f.SetGlobalRegistry(tt.registry(t, repo, first))

			ctx := tt.ctx
			if ctx == nil {
				ctx = t.Context()
			}

			got, err := f.Fetch(ctx, lockedWeb(repo, first))
			if err == nil {
				t.Fatalf("Fetch() past a registry that says nothing = commit %s, listing %v", got.Origin.Commit, got.Listing)
			}

			if !errors.Is(err, tt.expected) {
				t.Errorf("Fetch() = %v, want %v", err, tt.expected)
			}
		})
	}
}

// On a machine without a network the registry can't be reached at all
// (design 7.3): the mirror of an earlier run serves as the index, blocks
// included; without one, a version is fetched by its lock entry, without a
// listing.
func TestFetcher_GlobalFetch_WithoutNetwork(t *testing.T) {
	t.Parallel()

	t.Run("no mirror", func(t *testing.T) {
		t.Parallel()

		repo, first, _ := testRepo(t)

		f := New(t.TempDir())
		f.SetGlobalRegistry(noNetwork)

		got, err := f.Fetch(t.Context(), lockedWeb(repo, first))
		if err != nil {
			t.Fatalf("Fetch() by the lock entry, without a network = %v", err)
		}

		if got.Origin.Commit != first || got.Listing != nil {
			t.Errorf("Fetch() = commit %s, listing %v; want %s without a listing", got.Origin.Commit, got.Listing, first)
		}

		// What the lock file does not pin is not fetched without the index.
		unpinned := lockedWeb(repo, "")
		if _, err := f.Fetch(t.Context(), unpinned); err == nil {
			t.Error("Fetch() of a version no lock file pins, without the index = nil")
		}
	})

	t.Run("the mirror of an earlier run", func(t *testing.T) {
		t.Parallel()

		repo, first, _ := testRepo(t)

		listed := New(t.TempDir())
		mirrorOf(t, listed, noNetwork, registryFiles(t, repo, first, `{"advisories":[]}`))
		listed.SetGlobalRegistry(noNetwork)

		got, err := listed.Fetch(t.Context(), lockedWeb(repo, first))
		if err != nil {
			t.Fatalf("Fetch() with the mirror of an earlier run = %v", err)
		}

		if got.Listing == nil {
			t.Error("Fetch() with the mirror of an earlier run has no listing")
		}

		blocked := New(t.TempDir())
		mirrorOf(t, blocked, noNetwork, registryFiles(t, repo, first, blockedWeb))
		blocked.SetGlobalRegistry(noNetwork)

		if _, err := blocked.Fetch(t.Context(), lockedWeb(repo, first)); !errors.Is(err, ErrBlocked) {
			t.Errorf("Fetch() of a version the mirror blocks = %v, want ErrBlocked", err)
		}
	})
}

// The mirror of an earlier run serves as the index only when the registry
// can't be reached: a request that ended is not answered from it.
func TestFetcher_IndexFS_MirrorOnlyWithoutNetwork(t *testing.T) {
	t.Parallel()

	repo, first, _ := testRepo(t)

	f := New(t.TempDir())
	mirrorOf(t, f, noNetwork, registryFiles(t, repo, first, `{"advisories":[]}`))
	f.SetGlobalRegistry(noNetwork)

	ended, cancel := context.WithCancel(context.Background())
	cancel()

	req := source.Request{Source: webNamespace + "/" + webName, Kind: source.KindGlobal, Namespace: webNamespace, Name: webName}

	if got, err := f.Versions(ended, req); !errors.Is(err, context.Canceled) {
		t.Errorf("Versions() of a request that ended = %v, %v; want context.Canceled", got, err)
	}

	got, err := f.Versions(t.Context(), req)
	if err != nil || len(got) != 1 {
		t.Errorf("Versions() without a network, from the mirror = %v, %v", got, err)
	}

	if _, err := os.Stat(f.mirrorDir(noNetwork)); err != nil {
		t.Errorf("the mirror of the earlier run: %v", err)
	}
}

// A registry that does not answer holds the index of a fetcher for as long
// as it likes; a second reader of the same fetcher waits for it no longer
// than its own context.
func TestFetcher_ReadIndex_HangingRegistry(t *testing.T) {
	t.Parallel()

	accepted, release := make(chan struct{}), make(chan struct{})

	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(accepted) })
		<-release
	}))
	t.Cleanup(srv.Close)

	f := New(t.TempDir())
	f.SetGlobalRegistry(srv.URL + "/registry")

	req := source.Request{Source: webNamespace + "/" + webName, Kind: source.KindGlobal, Namespace: webNamespace, Name: webName}

	done := make(chan struct{})

	go func() {
		defer close(done)

		if _, err := f.Versions(t.Context(), req); err == nil {
			t.Error("Versions() of a registry that never answers = nil")
		}
	}()

	<-accepted

	t.Cleanup(func() {
		close(release)
		<-done
	})

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	answered := make(chan error, 1)

	go func() {
		_, err := f.Versions(ctx, req)
		answered <- err
	}()

	select {
	case err := <-answered:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Versions() of the second reader = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the second reader waits for the registry that does not answer, past its own deadline")
	}
}

// The index a fetcher keeps is of the registry it reads now: one that was
// being read when the address changed is not kept for the new address.
func TestFetcher_SetGlobalRegistry_WhileReading(t *testing.T) {
	t.Parallel()

	before, after := indexDir(t, testVersion), indexDir(t, "2.0.0")
	req := source.Request{Source: "norca/nats", Kind: source.KindGlobal, Namespace: webNamespace, Name: "nats"}

	f := New(t.TempDir())
	f.SetGlobalRegistry(before)

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			if _, err := f.Versions(t.Context(), req); err != nil {
				t.Errorf("Versions() while the address changes = %v", err)
			}
		})
	}

	f.SetGlobalRegistry(after)
	wg.Wait()

	got, err := f.Versions(t.Context(), req)
	if err != nil || len(got) != 1 || got[0].Version != "2.0.0" {
		t.Errorf("Versions() after the address changed = %v, %v; want 2.0.0 of the new registry", got, err)
	}
}
