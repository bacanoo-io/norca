// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/rigger/source"
)

// redirected is a repository whose info/refs redirects to another origin,
// which records what reaches it.
type redirected struct {
	// url is the address of the repository, before the redirect.
	url string

	mu       sync.Mutex
	requests int
	leaked   []string
}

// redirectedRepo starts a repository that redirects to another origin: the
// port of the other server on otherHost (localhost is another host,
// 127.0.0.1 another port of the same).
func redirectedRepo(t *testing.T, otherHost string) *redirected {
	t.Helper()

	out := &redirected{}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out.mu.Lock()
		defer out.mu.Unlock()

		out.requests++

		if r.Header.Get("Authorization") != "" {
			out.leaked = append(out.leaked, r.Method+" "+r.URL.Path)
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

	target := strings.Replace(other.URL, "127.0.0.1", otherHost, 1)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+"/repo/info/refs?service=git-upload-pack", http.StatusFound)
	}))
	t.Cleanup(first.Close)

	out.url = first.URL + "/repo"

	return out
}

// seen returns how many requests reached the other origin, and those of
// them that had an Authorization header.
func (r *redirected) seen() (requests int, leaked []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.requests, r.leaked
}

// The origins a redirect leads to in the tests of credentials.
var otherOrigins = []struct {
	name string
	host string
}{
	{name: "another host", host: "localhost"},
	{name: "another port", host: "127.0.0.1"},
}

// Credentials are for the origin they were given for (scheme, host and
// port): a redirect takes them nowhere else, not in the request that
// follows it and not in those after it.
func TestFetcher_RedirectKeepsCredentials(t *testing.T) {
	t.Parallel()

	for _, tt := range otherOrigins {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := redirectedRepo(t, tt.host)

			f := New(t.TempDir())
			f.SetCredentials(func(context.Context, string) (*Credentials, error) {
				return &Credentials{Password: "fixture"}, nil
			})

			if _, err := f.Sync(t.Context(), repo.url); err == nil {
				t.Error("Sync() of a repository that serves no pack = nil")
			}

			requests, leaked := repo.seen()

			if requests == 0 {
				t.Error("the redirect was not followed: the test shows nothing")
			}

			if len(leaked) > 0 {
				t.Errorf("the credentials reached another origin: %v", leaked)
			}
		})
	}
}

// A user and a password written into an address are no credentials of an
// origin: go-git would keep them over a redirect of info/refs and send them
// to where it led. Such an address is refused before a request is made, and
// its error does not repeat the secret.
func TestFetcher_RedirectWithCredentialsInAddress(t *testing.T) {
	t.Parallel()

	for _, tt := range otherOrigins {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := redirectedRepo(t, tt.host)
			address := secretAddress.Replace(strings.Replace(repo.url, "http://", "http://{bob:PW}", 1))

			_, err := New(t.TempDir()).Sync(t.Context(), address)
			if !errors.Is(err, source.ErrCredentials) {
				t.Errorf("Sync() of an address with a password = %v, want ErrCredentials", err)
			}

			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("the error repeats the password: %v", err)
			}

			if _, leaked := repo.seen(); len(leaked) > 0 {
				t.Errorf("the credentials of the address reached another origin: %v", leaked)
			}
		})
	}
}

// secretAddress writes a test address with credentials: {bob:PW} stands
// for bob:s3cret@, so that a linter reads no credentials in a URL.
var secretAddress = strings.NewReplacer("{", "", "}", "@", "PW", "s3cret")

// An address with credentials is refused by every way into the fetcher, and
// no error of it repeats them; the user of an ssh address is no secret and
// stays.
func TestFetcher_CredentialsInAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		address string
		refused bool
	}{
		{name: "http, a password", address: "http://{bob:PW}127.0.0.1:1/repo", refused: true},
		{name: "https, a token as the user", address: "https://{PW}127.0.0.1:1/repo", refused: true},
		{name: "a host and a path", address: "{bob:PW}127.0.0.1:1/repo", refused: true},
		{name: "ssh, a password", address: "ssh://{git:PW}127.0.0.1:1/repo", refused: true},
		{name: "https, anonymous", address: "https://127.0.0.1:1/repo"},
		{name: "ssh, a user", address: "ssh://{git}127.0.0.1:1/repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			address := secretAddress.Replace(tt.address)

			f := New(t.TempDir())
			f.SetGlobalRegistry(address)

			commit := strings.Repeat("a", 40)

			calls := map[string]func() error{
				"Sync": func() error {
					_, err := f.Sync(t.Context(), address)

					return err
				},
				"Tags": func() error {
					_, err := f.Tags(t.Context(), address)

					return err
				},
				"Checkout": func() error {
					_, err := f.Checkout(t.Context(), address, commit)

					return err
				},
				"Fetch": func() error {
					_, err := f.Fetch(t.Context(), source.Request{Kind: source.KindGit, Repository: address, Path: ".", Ref: testRef})

					return err
				},
				"the global registry": func() error {
					_, err := f.Versions(t.Context(), source.Request{Kind: source.KindGlobal, Namespace: "norca", Name: "web"})

					return err
				},
			}

			for name, call := range calls {
				err := call()
				if err == nil {
					t.Fatalf("%s of a repository no one serves = nil", name)
				}

				if got := errors.Is(err, source.ErrCredentials); got != tt.refused {
					t.Errorf("%s: refused for its credentials = %v, want %v: %v", name, got, tt.refused, err)
				}

				if strings.Contains(err.Error(), "s3cret") {
					t.Errorf("%s: the error repeats the secret: %v", name, err)
				}
			}
		})
	}
}
