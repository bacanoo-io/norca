// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/bacanoo-io/norca/rigger/internal/sameorigin"
)

// Credentials over a redirect. After a redirect of info/refs go-git talks
// to where it led and sets the authentication of the first address on each
// request there, and net/http copies the header to a redirect on the same
// host name whatever its scheme or port. So the authentication sets itself
// only on requests to its origin (originAuth), and the http client of
// go-git drops the header from a redirect to another (sameorigin).

// originAuth is basic authentication for one origin.
type originAuth struct {
	*githttp.BasicAuth

	origin string
}

var _ githttp.AuthMethod = (*originAuth)(nil)

func newOriginAuth(repository, address string, basic *githttp.BasicAuth) (*originAuth, error) {
	u, err := url.Parse(address)
	if err != nil {
		// The error of url.Parse quotes the address, which may hold a secret.
		return nil, fmt.Errorf("%s: credentials need a URL of a host", repository)
	}

	return &originAuth{BasicAuth: basic, origin: sameorigin.Of(u)}, nil
}

// SetAuth sets the authentication on a request to its origin, on no other.
func (a *originAuth) SetAuth(r *http.Request) {
	if sameorigin.Of(r.URL) == a.origin {
		a.BasicAuth.SetAuth(r)
	}
}

// installKeepOrigin gives go-git, whose clients are the process's, a client
// that keeps credentials to their origin. A client that a host installed
// stays.
var installKeepOrigin = sync.OnceFunc(func() {
	c := githttp.NewClient(&http.Client{Transport: http.DefaultTransport, CheckRedirect: sameorigin.CheckRedirect})

	for _, scheme := range []string{"http", "https"} {
		if client.Protocols[scheme] == githttp.DefaultClient {
			client.InstallProtocol(scheme, c)
		}
	}
})
