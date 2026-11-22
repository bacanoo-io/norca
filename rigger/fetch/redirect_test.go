// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Credentials are for the origin they were given for (scheme, host and
// port): a redirect takes them nowhere else, not in the request that
// follows it and not in those after it.
func TestFetcher_RedirectKeepsCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
	}{
		{name: "another host", host: "localhost"},
		{name: "another port", host: "127.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				mu       sync.Mutex
				requests int
				leaked   []string
			)

			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()

				requests++

				if r.Header.Get("Authorization") != "" {
					leaked = append(leaked, r.Method+" "+r.URL.Path)
				}

				if r.Method != http.MethodGet {
					w.WriteHeader(http.StatusInternalServerError)

					return
				}

				w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")

				service := "# service=git-upload-pack\n"
				ref := strings.Repeat("a", 40) + " refs/heads/main\x00ofs-delta\n"
				_, _ = fmt.Fprintf(w, "%04x%s0000%04x%s0000", len(service)+4, service, len(ref)+4, ref)
			}))
			t.Cleanup(other.Close)

			target := strings.Replace(other.URL, "127.0.0.1", tt.host, 1)

			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target+"/repo/info/refs?service=git-upload-pack", http.StatusFound)
			}))
			t.Cleanup(first.Close)

			f := New(t.TempDir())
			f.SetCredentials(func(context.Context, string) (*Credentials, error) {
				return &Credentials{Password: "fixture"}, nil
			})

			if _, err := f.Sync(t.Context(), first.URL+"/repo"); err == nil {
				t.Error("Sync() of a repository that serves no pack = nil")
			}

			mu.Lock()
			defer mu.Unlock()

			if requests == 0 {
				t.Error("the redirect was not followed: the test shows nothing")
			}

			if len(leaked) > 0 {
				t.Errorf("the credentials reached another origin: %v", leaked)
			}
		})
	}
}
