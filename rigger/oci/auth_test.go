// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/internal/readlimit"
)

const testRegistry = "registry.example.com"

type roundTrip func(*http.Request) *http.Response

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}

	resp := f(r)
	resp.Request = r

	return resp, nil
}

// answer is an answer of a transport that reaches no network; header is
// key, value, key, value.
func answer(status int, body string, header ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}

	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func testClient(rt roundTrip) *Client {
	c := NewClient()
	h := *c.http
	h.Transport = rt
	c.http = &h

	return c
}

// The credentials of a registry stay with it and with https: not a token
// service over http, not where an upload or a redirect leads, and not a
// host that reads as localhost and is another.
func TestClient_CredentialsStay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		challenge string
		call      func(c *Client) error
		// trusted is the one origin that may see credentials.
		trusted string
	}{
		{
			name:      "token service over http",
			challenge: `Bearer realm="http://auth.example.com/token"`,
			call: func(c *Client) error {
				_, err := c.Tags(t.Context(), testRegistry+"/acme/web")

				return err
			},
			trusted: "https://" + testRegistry,
		},
		{
			name:      "upload elsewhere",
			challenge: "Basic",
			call:      func(c *Client) error { return c.pushBlob(t.Context(), testRegistry+"/acme/web", []byte("layer")) },
			trusted:   "https://" + testRegistry,
		},
		{
			name:      "redirect to another port",
			challenge: "Basic",
			call: func(c *Client) error {
				_, err := c.Tags(t.Context(), testRegistry+"/acme/web")

				return err
			},
			trusted: "https://" + testRegistry,
		},
		{
			name:      "host that reads as localhost",
			challenge: "Basic",
			call: func(c *Client) error {
				_, err := c.Tags(t.Context(), "127.0.0.1:1@"+testRegistry+"/acme/web")

				return err
			},
			trusted: "https://" + testRegistry,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var leaked []string

			c := testClient(func(r *http.Request) *http.Response {
				at := r.URL.Scheme + "://" + r.URL.Host
				auth := r.Header.Get("Authorization")

				if auth != "" && at != tt.trusted {
					leaked = append(leaked, r.Method+" "+at)
				}

				switch {
				case at != tt.trusted && auth == "" && strings.HasPrefix(tt.name, "host"):
					return answer(http.StatusUnauthorized, "", "WWW-Authenticate", tt.challenge)
				case at != tt.trusted:
					return answer(http.StatusCreated, `{"token":"t"}`)
				case auth == "":
					return answer(http.StatusUnauthorized, "", "WWW-Authenticate", tt.challenge)
				case r.Method == http.MethodHead:
					return answer(http.StatusNotFound, "")
				case r.Method == http.MethodPost:
					return answer(http.StatusAccepted, "", "Location", "https://storage.example.net/upload")
				default:
					return answer(http.StatusFound, "", "Location", "http://"+testRegistry+":81/elsewhere")
				}
			})

			// Credentials for every host: what is checked is where the
			// client sends those it has.
			c.SetCredentials(func(string) (string, string, bool, error) { return "robot", "fixture", true, nil })

			err := tt.call(c)

			if len(leaked) > 0 {
				t.Errorf("the credentials reached %v (err = %v)", leaked, err)
			}
		})
	}
}

// The answer of a token service is read to a bound.
func TestClient_TokenBounded(t *testing.T) {
	t.Parallel()

	c := testClient(func(r *http.Request) *http.Response {
		if strings.HasSuffix(r.URL.Path, "/token") {
			return answer(http.StatusOK, `{"pad":"`+strings.Repeat("x", 2<<20)+`","token":"t"}`)
		}

		if r.Header.Get("Authorization") == "" {
			return answer(http.StatusUnauthorized, "", "WWW-Authenticate", `Bearer realm="https://`+testRegistry+`/token"`)
		}

		return answer(http.StatusOK, `{"tags":[]}`)
	})

	if _, err := c.Tags(t.Context(), testRegistry+"/acme/web"); !errors.Is(err, readlimit.ErrTooLarge) {
		t.Errorf("Tags() = %v, want the answer of the token service refused", err)
	}
}
