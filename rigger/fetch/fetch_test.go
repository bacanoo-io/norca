// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/rigger/internal/targz/targztest"
	"github.com/bacanoo-io/norca/rigger/source"
)

// The cache serves in the place of a repository or a registry only when
// that can't be reached. One that answers and refuses the caller, or says
// that it has no such thing, is the answer, and so is a request that ended:
// a caller without the credentials of a private repository does not get
// from the cache what another caller fetched with them.
func TestFetcher_CacheOnlyWhenUnreachable(t *testing.T) {
	t.Parallel()

	commit, manifest := strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64)

	ended, cancel := context.WithCancel(context.Background())
	cancel()

	answers := []struct {
		name string
		// status is what the server answers every request with; 0 is a
		// server that can't be reached.
		status int
		ctx    context.Context //nolint:containedctx // the context of a case
		served bool
	}{
		{name: "credentials required", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "not found", status: http.StatusNotFound},
		{name: "a server error", status: http.StatusInternalServerError},
		{name: "a request that ended", ctx: ended},
		{name: "no network", served: true},
	}

	kinds := []struct {
		name string
		// req is the request of a locked version of the repository at an
		// address (a host, a port and a path), and dir is where the cache
		// keeps it.
		req func(address string) source.Request
		dir func(f *Fetcher, req source.Request) string
	}{
		{
			name: "tree",
			req: func(address string) source.Request {
				return source.Request{Kind: source.KindGit, Repository: "http://" + address, Ref: testRef, Commit: commit, Path: testDir}
			},
			dir: func(f *Fetcher, req source.Request) string { return f.TreeDir(req.Repository, commit) },
		},
		{
			name: "layer",
			req: func(address string) source.Request {
				return source.Request{Kind: source.KindOCI, Repository: address, Version: testVersion, Commit: manifest, Path: testDir}
			},
			dir: func(f *Fetcher, req source.Request) string { return f.OCIDir(req.Repository, manifest) },
		},
	}

	for _, answer := range answers {
		for _, kind := range kinds {
			t.Run(answer.name+", "+kind.name, func(t *testing.T) {
				t.Parallel()

				address := "127.0.0.1:1/private"

				if answer.status != 0 {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
						w.WriteHeader(answer.status)
					}))
					t.Cleanup(srv.Close)

					address = strings.TrimPrefix(srv.URL, "http://") + "/private"
				}

				f := New(t.TempDir())
				req := kind.req(address)

				write(t, filepath.Join(kind.dir(f, req), testDir, "metadata.hcl"), "module {}\n")

				ctx := answer.ctx
				if ctx == nil {
					ctx = t.Context()
				}

				_, err := f.Fetch(ctx, req)
				if served := err == nil; served != answer.served {
					t.Errorf("Fetch() served from the cache = %v, want %v (err = %v)", served, answer.served, err)
				}
			})
		}
	}
}

// A repository on the local file system whose directory this machine does
// not have is one that can't be reached (a stack brought here with its
// bundle); a directory that is there and is no repository is an error.
func TestFetcher_CacheOfALocalRepository(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)

	tests := []struct {
		name   string
		make   func(t *testing.T, dir string)
		served bool
	}{
		{name: "no directory", make: func(*testing.T, string) {}, served: true},
		{name: "a directory that is no repository", make: func(t *testing.T, dir string) {
			t.Helper()
			write(t, filepath.Join(dir, "README.md"), "no repository\n")
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := filepath.Join(t.TempDir(), "repo")
			tt.make(t, repo)

			f := New(t.TempDir())
			req := source.Request{Kind: source.KindGit, Repository: repo, Ref: testRef, Commit: commit, Path: testDir}

			write(t, filepath.Join(f.TreeDir(repo, commit), testDir, "metadata.hcl"), "module {}\n")

			_, err := f.Fetch(t.Context(), req)
			if served := err == nil; served != tt.served {
				t.Errorf("Fetch() served from the cache = %v, want %v (err = %v)", served, tt.served, err)
			}
		})
	}
}

// A sparse file in the layer of an OCI module would fill the disk: the bound
// of a layer counts its bytes, not what they write.
func TestUntar_Sparse(t *testing.T) {
	t.Parallel()

	if err := untar(targztest.SparseTar("big", 8<<20), t.TempDir()); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Errorf("untar() = %v, want the sparse file refused", err)
	}
}

// A repository that does not answer holds up only itself, and a caller
// waits for it no longer than its own context: a server has one fetcher.
func TestFetcher_HangingRepository(t *testing.T) {
	t.Parallel()

	accepted, release := make(chan struct{}), make(chan struct{})

	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(accepted) })
		<-release
	}))
	t.Cleanup(srv.Close)

	f := New(t.TempDir())
	hanging := source.Request{Kind: source.KindGit, Repository: srv.URL + "/hang"}

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, err := f.Versions(t.Context(), hanging)
		if err == nil {
			t.Error("Versions() of a repository that never answers = nil")
		}
	}()

	<-accepted

	t.Cleanup(func() {
		close(release)
		<-done
	})

	for name, req := range map[string]source.Request{
		"another repository":        {Kind: source.KindGit, Repository: "http://127.0.0.1:1/other"},
		"the same, with a deadline": hanging,
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)

		answered := make(chan struct{})

		go func() {
			defer close(answered)

			if _, err := f.Versions(ctx, req); err == nil {
				t.Errorf("%s: Versions() = nil", name)
			}
		}()

		select {
		case <-answered:
		case <-time.After(5 * time.Second):
			t.Errorf("%s waits for the repository that does not answer", name)
		}

		cancel()
	}
}
