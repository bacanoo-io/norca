// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package redact

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestURLError(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, URLError(nil))

	plain := errors.New("plain")
	must.Eq(t, plain, URLError(plain))

	reason := errors.New("connection refused")
	urlErr := &url.Error{Op: "Post", URL: "https://sk_live_SECRET@hooks.example.com/x?key=SECRET", Err: reason}
	must.Eq(t, reason, URLError(urlErr))
	must.Eq(t, reason, URLError(fmt.Errorf("wrapped: %w", urlErr)))
	must.Eq(t, reason, URLError(&url.Error{Op: "Get", URL: "https://a", Err: urlErr}))

	// what a real client returns: the reason is kept for errors.Is
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://SECRETTOKEN@127.0.0.1:1/x?key=SECRETVALUE", nil)
	must.NoError(t, err)
	_, err = (&http.Client{Timeout: time.Second}).Do(req)
	must.Error(t, err)
	must.StrContains(t, err.Error(), "SECRETTOKEN")
	clean := URLError(err)
	must.ErrorIs(t, clean, context.Canceled)
	must.StrNotContains(t, clean.Error(), "SECRET")
}

func TestURLSecrets(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name, text, want string
		urls             []string
	}{
		{
			name: "no credentials",
			text: `Get "https://h.example.com/a": dial tcp 10.0.0.1:443: connect: connection refused`,
			want: `Get "https://h.example.com/a": dial tcp 10.0.0.1:443: connect: connection refused`,
			urls: []string{"https://h.example.com/a"},
		},
		{
			name: "token as the user name",
			text: `Post "https://sk_live_SECRET@hooks.example.com/x": EOF`,
			want: `Post "https://xxxxx@hooks.example.com/x": EOF`,
			urls: []string{"https://sk_live_SECRET@hooks.example.com/x"},
		},
		{
			name: "user and password, as Go prints them",
			text: `Post "https://TOKEN:xxxxx@hooks.example.com/x": EOF`,
			want: `Post "https://xxxxx@hooks.example.com/x": EOF`,
			urls: []string{"https://TOKEN:x-oauth-basic@hooks.example.com/x"},
		},
		{
			name: "user and password, as the errors of net/http print them",
			text: `get keys failed Get "http://TOKEN:***@127.0.0.1:1/keys": dial tcp 127.0.0.1:1: connect: connection refused`,
			want: `get keys failed Get "http://xxxxx@127.0.0.1:1/keys": dial tcp 127.0.0.1:1: connect: connection refused`,
			urls: []string{"http://TOKEN:pass@127.0.0.1:1/keys"},
		},
		{
			name: "a URL that can't be parsed",
			text: "parse \"https://TOKEN:pa ss@hooks.example.com/x\x7f\": invalid",
			want: `parse "<redacted>": invalid`,
			urls: []string{"https://TOKEN:pa ss@hooks.example.com/x\x7f"},
		},
		{
			name: "password only",
			text: `Get "https://:xxxxx@h.example.com/": EOF and https://:SECRET@h.example.com/`,
			want: `Get "https://xxxxx@h.example.com/": EOF and https://xxxxx@h.example.com/`,
			urls: []string{"https://:SECRET@h.example.com/"},
		},
		{
			name: "escaped user name",
			text: `Get "https://to%40ken@h.example.com/": EOF, to@ken@h`,
			want: `Get "https://xxxxx@h.example.com/": EOF, xxxxx@h`,
			urls: []string{"https://to%40ken@h.example.com/"},
		},
		{
			name: "query values, with a path appended by the client",
			text: `Post "https://prom.example.com/api/v1/query?apikey=SECRET&x=a%2Fb&bare": EOF`,
			want: `Post "https://prom.example.com/api/v1/query?apikey=<redacted>&x=<redacted>&<redacted>": EOF`,
			urls: []string{"https://prom.example.com?apikey=SECRET&x=a%2Fb&bare"},
		},
		{
			name: "query value decoded and fragment",
			text: `x=a/b y=1 #frag`,
			want: `x=<redacted> y=1 #<redacted>`,
			urls: []string{"https://h.example.com/?x=a%2Fb#frag"},
		},
		{
			name: "several URLs, empty ones skipped",
			text: `a https://one@h1/ b https://two@h2/`,
			want: `a https://xxxxx@h1/ b https://xxxxx@h2/`,
			urls: []string{"", "https://one@h1/", "https://two@h2/"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must.Eq(t, tc.want, URLSecrets(tc.text, tc.urls...))
		})
	}
}

func TestCallError(t *testing.T) {
	ci.Parallel(t)

	must.NoError(t, CallError(nil, "https://tok@h"))

	// the error of net/http: the reason is kept
	reason := errors.New("connection refused")
	err := CallError(&url.Error{Op: "Post", URL: "https://TOK@h.example.com/x", Err: reason}, "https://TOK@h.example.com/x")
	must.Eq(t, reason, err)

	// a library that formatted the URL into its own text
	err = CallError(fmt.Errorf(`fetching keys: Get "https://TOK@h.example.com/x?k=SECRET": EOF`), "https://TOK@h.example.com/x?k=SECRET")
	must.EqError(t, err, `fetching keys: Get "https://xxxxx@h.example.com/x?k=<redacted>": EOF`)

	// a real failed call with a URL nobody listens on
	const raw = "http://SECRETTOKEN:SECRETPASS@127.0.0.1:1/hook?key=SECRETVALUE"
	req, err := http.NewRequest(http.MethodPost, raw, strings.NewReader("{}"))
	must.NoError(t, err)
	_, err = (&http.Client{Timeout: 5 * time.Second}).Do(req)
	must.Error(t, err)
	must.StrContains(t, err.Error(), "SECRETTOKEN")
	must.StrContains(t, err.Error(), "SECRETVALUE")
	clean := CallError(err, raw)
	must.StrNotContains(t, clean.Error(), "SECRET")
	must.StrNotContains(t, fmt.Sprintf("%v %+v %#v", clean, clean, clean.Error()), "SECRET")
}
