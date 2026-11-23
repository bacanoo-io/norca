// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// A repository is a host and a name. One written with a user and a
// password is refused before a request, and the error does not repeat them.
func TestRef_Credentials(t *testing.T) {
	t.Parallel()

	requests := 0

	c := testClient(func(*http.Request) *http.Response {
		requests++

		return answer(http.StatusOK, `{"tags":[]}`)
	})

	_, err := c.Tags(t.Context(), "bob:s3cret@"+testRegistry+"/acme/web")
	if !errors.Is(err, ErrRegistry) {
		t.Errorf("Tags() = %v, want ErrRegistry", err)
	}

	if err != nil && strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error repeats the password: %v", err)
	}

	if requests != 0 {
		t.Errorf("%d requests were sent", requests)
	}
}

// The registry of a host is read over http when the host is this machine,
// whatever the form of its address, and over https otherwise.
func TestBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host     string
		expected string
	}{
		{host: "localhost", expected: "http://localhost"},
		{host: "localhost:5000", expected: "http://localhost:5000"},
		{host: "127.0.0.1:5000", expected: "http://127.0.0.1:5000"},
		{host: "127.0.0.2", expected: "http://127.0.0.2"},
		{host: "[::1]:5000", expected: "http://[::1]:5000"},
		{host: "[::1]", expected: "http://[::1]"},
		{host: testRegistry, expected: "https://" + testRegistry},
		{host: testRegistry + ":5000", expected: "https://" + testRegistry + ":5000"},
		{host: "localhost.example.com", expected: "https://localhost.example.com"},
		{host: "10.0.0.5:5000", expected: "https://10.0.0.5:5000"},
		{host: "[2001:db8::1]:5000", expected: "https://[2001:db8::1]:5000"},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()

			if got := baseURL(tt.host); got != tt.expected {
				t.Errorf("baseURL(%q) = %q, want %q", tt.host, got, tt.expected)
			}
		})
	}
}

// What a registry names may be this machine in more forms than a registry
// of this machine is named by.
func TestMayBeLocal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		expected bool
	}{
		{name: "localhost", expected: true},
		{name: "LOCALHOST", expected: true},
		{name: "localhost.", expected: true},
		{name: "db.localhost", expected: true},
		{name: "127.0.0.1", expected: true},
		{name: "127.8.9.10", expected: true},
		{name: "::1", expected: true},
		{name: "::ffff:127.0.0.1", expected: true},
		{name: "0.0.0.0", expected: true},
		{name: "::", expected: true},
		{name: "127.1", expected: true},
		{name: "2130706433", expected: true},
		{name: "0x7f.0.0.1", expected: true},
		{name: "0x7f000001", expected: true},
		{name: ""},
		{name: testRegistry},
		{name: "localhost.example.com"},
		{name: "notlocalhost"},
		{name: "10.0.0.5"},
		{name: "2001:db8::1"},
		{name: "registry1.example.io"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := mayBeLocal(tt.name); got != tt.expected {
				t.Errorf("mayBeLocal(%q) = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}

// internalService is a service of this machine that a registry elsewhere
// must not make the client call.
const internalService = "127.0.0.1:8500"

// A registry elsewhere does not lead the client to this machine: not by
// the realm of its token service, not by a redirect, not by where an upload
// goes. What such a service answers would go back to the registry as a
// token.
func TestClient_RemoteRegistryToLocalhost(t *testing.T) {
	t.Parallel()

	tags := func(c *Client) error {
		_, err := c.Tags(t.Context(), testRegistry+"/acme/web")

		return err
	}

	tests := []struct {
		name string
		// answer is what the registry or the token service of the case
		// says; nil goes on to the default answers.
		answer func(r *http.Request) *http.Response
		call   func(c *Client) error
	}{
		{
			name: "a realm over http",
			answer: func(r *http.Request) *http.Response {
				return challenge(r, `Bearer realm="http://`+internalService+`/v1/internal/thing?x=1",service="s"`)
			},
			call: tags,
		},
		{
			name: "a realm over https",
			answer: func(r *http.Request) *http.Response {
				return challenge(r, `Bearer realm="https://localhost:8500/token"`)
			},
			call: tags,
		},
		{
			name: "a realm on the loopback address of IPv6",
			answer: func(r *http.Request) *http.Response {
				return challenge(r, `Bearer realm="https://[::1]:8500/token"`)
			},
			call: tags,
		},
		{
			name: "a token service that redirects",
			answer: func(r *http.Request) *http.Response {
				if r.URL.Host == "auth.example.com" {
					return answer(http.StatusFound, "", "Location", "http://"+internalService+"/v1/internal/thing")
				}

				return challenge(r, `Bearer realm="https://auth.example.com/token"`)
			},
			call: tags,
		},
		{
			name: "a registry that redirects",
			answer: func(r *http.Request) *http.Response {
				if r.URL.Host == testRegistry {
					return answer(http.StatusFound, "", "Location", "http://"+internalService+"/v1/internal/thing")
				}

				return nil
			},
			call: tags,
		},
		{
			name: "an upload",
			answer: func(r *http.Request) *http.Response {
				switch {
				case r.URL.Host != testRegistry:
					return nil
				case r.Method == http.MethodHead:
					return answer(http.StatusNotFound, "")
				case r.Method == http.MethodPost:
					return answer(http.StatusAccepted, "", "Location", "http://"+internalService+"/v1/internal/thing")
				default:
					return nil
				}
			},
			call: func(c *Client) error { return c.pushBlob(t.Context(), testRegistry+"/acme/web", []byte("layer")) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var hit, tokens []string

			c := testClient(func(r *http.Request) *http.Response {
				if isLocalhost(r.URL.Hostname()) {
					hit = append(hit, r.Method+" "+r.URL.String())

					return answer(http.StatusOK, `{"access_token":"INTERNAL"}`)
				}

				if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
					tokens = append(tokens, auth)
				}

				if resp := tt.answer(r); resp != nil {
					return resp
				}

				return answer(http.StatusOK, `{"tags":[]}`)
			})

			c.SetCredentials(func(string) (string, string, bool, error) { return "robot", "fixture", true, nil })

			err := tt.call(c)
			if err == nil {
				t.Error("the call of a registry that leads to this machine = nil")
			}

			if len(hit) > 0 {
				t.Errorf("a registry elsewhere made the client call this machine: %v (err = %v)", hit, err)
			}

			if len(tokens) > 0 {
				t.Errorf("the registry got an answer of this machine as a token: %v", tokens)
			}
		})
	}
}

// challenge answers a request without credentials with a challenge, and
// nil for one that has them.
func challenge(r *http.Request, header string) *http.Response {
	if r.Header.Get("Authorization") != "" {
		return nil
	}

	return answer(http.StatusUnauthorized, "", "WWW-Authenticate", header)
}

// A registry of this machine has its token service and its uploads on this
// machine, over http: nothing of that is refused.
func TestClient_LocalRegistry(t *testing.T) {
	t.Parallel()

	for _, host := range []string{"127.0.0.1:5000", "localhost:5000", "[::1]:5000"} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()

			var uploaded bool

			c := testClient(func(r *http.Request) *http.Response {
				switch {
				case r.URL.Scheme != "http":
					return answer(http.StatusBadGateway, "")
				case r.URL.Port() == "5001" && r.URL.Path == "/token":
					return answer(http.StatusOK, `{"token":"t"}`)
				case r.URL.Port() == "5002":
					uploaded = r.Method == http.MethodPut

					return answer(http.StatusCreated, "")
				case r.Header.Get("Authorization") != "Bearer t":
					return answer(http.StatusUnauthorized, "", "WWW-Authenticate", `Bearer realm="http://localhost:5001/token"`)
				case r.Method == http.MethodHead:
					return answer(http.StatusNotFound, "")
				case r.Method == http.MethodPost:
					return answer(http.StatusAccepted, "", "Location", "http://127.0.0.1:5002/upload")
				default:
					return answer(http.StatusOK, `{"tags":["1.0.0"]}`)
				}
			})

			tags, err := c.Tags(t.Context(), host+"/acme/web")
			if err != nil || len(tags) != 1 {
				t.Errorf("Tags() of a registry of this machine = %v, %v", tags, err)
			}

			if err := c.pushBlob(t.Context(), host+"/acme/web", []byte("layer")); err != nil || !uploaded {
				t.Errorf("pushBlob() to a registry of this machine = %v, uploaded %v", err, uploaded)
			}
		})
	}
}

// The path of the list of tags of the test repository.
const tagsPath = "/v2/acme/web/tags/list"

// A registry gives the tags of a repository in pages, each with a Link to
// the next: Tags reads them all, so that the newest version is among them.
func TestClient_Tags_Pages(t *testing.T) {
	t.Parallel()

	// pages are the tags and the Link header of each page, by the query
	// of its request.
	tests := []struct {
		name     string
		pages    map[string][2]string
		expected string
	}{
		{
			name:     "one page",
			pages:    map[string][2]string{"": {`["1.0.0","1.0.1"]`, ""}},
			expected: "1.0.0 1.0.1",
		},
		{
			name: "a Link with a path",
			pages: map[string][2]string{
				"":               {`["1.0.0","1.0.1"]`, `<` + tagsPath + `?last=1.0.1&n=2>; rel="next"`},
				"last=1.0.1&n=2": {`["1.1.0","1.2.0"]`, `<` + tagsPath + `?last=1.2.0&n=2>; rel="next"`},
				"last=1.2.0&n=2": {`["2.0.0"]`, ""},
				"last=2.0.0&n=2": {`["9.9.9"]`, ""},
				"n=2&last=1.0.1": {`["9.9.9"]`, ""},
			},
			expected: "1.0.0 1.0.1 1.1.0 1.2.0 2.0.0",
		},
		{
			name: "a Link with a URL of the registry",
			pages: map[string][2]string{
				"":           {`["1.0.0"]`, `<https://` + testRegistry + tagsPath + `?last=1.0.0>; rel="next"`},
				"last=1.0.0": {`["2.0.0"]`, ""},
			},
			expected: "1.0.0 2.0.0",
		},
		{
			name: "a Link among others, without quotes",
			pages: map[string][2]string{
				"":           {`["1.0.0"]`, `<` + tagsPath + `>; rel="first", <` + tagsPath + `?last=1.0.0>; rel=next; title="a, b"`},
				"last=1.0.0": {`["2.0.0"]`, `<` + tagsPath + `>; rel="first"`},
			},
			expected: "1.0.0 2.0.0",
		},
		{
			name: "a last page without tags",
			pages: map[string][2]string{
				"":           {`["1.0.0"]`, `<` + tagsPath + `?last=1.0.0>; rel="next"`},
				"last=1.0.0": {`null`, ""},
			},
			expected: "1.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := testClient(func(r *http.Request) *http.Response {
				page, ok := tt.pages[r.URL.RawQuery]
				if !ok || r.URL.Host != testRegistry || r.URL.Path != tagsPath {
					return answer(http.StatusNotFound, "")
				}

				return answer(http.StatusOK, `{"name":"acme/web","tags":`+page[0]+`}`, "Link", page[1])
			})

			tags, err := c.Tags(t.Context(), testRegistry+"/acme/web")
			if err != nil {
				t.Fatalf("Tags() = %v", err)
			}

			if got := strings.Join(tags, " "); got != tt.expected {
				t.Errorf("Tags() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// The next page of tags is of the same list of the same registry, and the
// pages end: a registry that says otherwise gets an error, not a list cut
// short, and leads the client nowhere.
func TestClient_Tags_PagesRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// link is the Link header of a page, given the number of the page.
		link func(page int) string
	}{
		{name: "another host", link: func(int) string { return `<https://elsewhere.example.net` + tagsPath + `?last=1.0.0>; rel="next"` }},
		{name: "another port", link: func(int) string { return `<https://` + testRegistry + `:8443` + tagsPath + `?last=1.0.0>; rel="next"` }},
		{name: "http", link: func(int) string { return `<http://` + testRegistry + tagsPath + `?last=1.0.0>; rel="next"` }},
		{name: "another path", link: func(int) string { return `</v2/acme/private/tags/list?last=1.0.0>; rel="next"` }},
		{name: "the same page again", link: func(int) string { return `<` + tagsPath + `>; rel="next"` }},
		{name: "pages without an end", link: func(page int) string {
			return `<` + tagsPath + `?last=1.0.` + strconv.Itoa(page) + `>; rel="next"`
		}},
		{name: "no URL", link: func(int) string { return `<://>; rel="next"` }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				pages     int
				elsewhere []string
			)

			c := testClient(func(r *http.Request) *http.Response {
				if r.URL.Scheme != "https" || r.URL.Host != testRegistry || r.URL.Path != tagsPath {
					elsewhere = append(elsewhere, r.URL.String())

					return answer(http.StatusOK, `{"tags":["6.6.6"]}`)
				}

				pages++

				return answer(http.StatusOK, `{"tags":["1.0.`+strconv.Itoa(pages)+`"]}`, "Link", tt.link(pages))
			})

			tags, err := c.Tags(t.Context(), testRegistry+"/acme/web")
			if !errors.Is(err, ErrRegistry) {
				t.Errorf("Tags() = %d tags, %v; want ErrRegistry", len(tags), err)
			}

			if len(elsewhere) > 0 {
				t.Errorf("the client was led to %v", elsewhere)
			}

			if pages > maxTagPages {
				t.Errorf("%d pages were read, more than the bound %d", pages, maxTagPages)
			}
		})
	}
}

// What is no list of tags is an error, not an empty list.
func TestClient_Tags_Invalid(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"<html>", `{"tags":"1.0.0"}`, ""} {
		c := testClient(func(*http.Request) *http.Response { return answer(http.StatusOK, body) })

		if tags, err := c.Tags(t.Context(), testRegistry+"/acme/web"); err == nil {
			t.Errorf("Tags() of the answer %q = %v, want an error", body, tags)
		}
	}
}

// The Location of an upload is a URL, or a reference from the request that
// started the upload: a name alone is beside that request, not at the root
// of the registry.
func TestClient_PushBlob_Location(t *testing.T) {
	t.Parallel()

	const uploads = "https://" + testRegistry + "/v2/acme/web/blobs/uploads/"

	tests := []struct {
		name     string
		location string
		// expected is where the blob is put, without its query.
		expected string
		query    string
	}{
		{name: "a name", location: "upload-1?state=abc", expected: uploads + "upload-1", query: "abc"},
		{name: "a path", location: "/v2/acme/web/blobs/uploads/upload-2?state=abc", expected: uploads + "upload-2", query: "abc"},
		{name: "a URL of the registry", location: uploads + "upload-3", expected: uploads + "upload-3"},
		{name: "a URL of a store", location: "https://storage.example.net/upload-4?state=abc", expected: "https://storage.example.net/upload-4", query: "abc"},
		{name: "none", location: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var put []string

			c := testClient(func(r *http.Request) *http.Response {
				switch r.Method {
				case http.MethodHead:
					return answer(http.StatusNotFound, "")
				case http.MethodPost:
					return answer(http.StatusAccepted, "", "Location", tt.location)
				default:
					put = append(put, r.URL.Scheme+"://"+r.URL.Host+r.URL.Path+" state="+r.URL.Query().Get("state")+" digest="+r.URL.Query().Get("digest")[:7])

					return answer(http.StatusCreated, "")
				}
			})

			err := c.pushBlob(t.Context(), testRegistry+"/acme/web", []byte("layer"))

			if tt.expected == "" {
				if !errors.Is(err, ErrRegistry) || len(put) != 0 {
					t.Errorf("pushBlob() without a Location = %v, put to %v; want ErrRegistry and no upload", err, put)
				}

				return
			}

			want := tt.expected + " state=" + tt.query + " digest=sha256:"
			if err != nil || len(put) != 1 || put[0] != want {
				t.Errorf("pushBlob() put the blob to %v (err = %v), want %q", put, err, want)
			}
		})
	}
}
