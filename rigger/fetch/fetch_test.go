// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/rigger/internal/targz/targztest"
	"github.com/bacanoo-io/norca/rigger/source"
)

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
