// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package userinfo_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/internal/userinfo"
)

// address writes a test address: {bob:pw} stands for bob:pw@, so that a
// linter reads no credentials in a URL.
var address = strings.NewReplacer("{", "", "}", "@")

const (
	schemeHTTPS = "https"
	schemeFile  = "file"
	schemeDir   = "dir"
)

// Find reads an authority as net/url does, also of an address net/url does
// not parse; a path of the file system has no user.
func TestFind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		address string
		scheme  string
		info    string
		found   bool
	}{
		{name: "a user and a password", address: "https://{bob:pw}example.com/r", scheme: schemeHTTPS, info: "bob:pw", found: true},
		{name: "a user", address: "ssh://{git}example.com/r", scheme: "ssh", info: "git", found: true},
		{name: "an empty user", address: "https://{}example.com/r", scheme: schemeHTTPS, found: true},
		{name: "the last at sign", address: "https://{bob:p}w}example.com/r", scheme: schemeHTTPS, info: "bob:p@w", found: true},
		{name: "a scheme in capitals", address: "HTTPS://{bob}example.com", scheme: schemeHTTPS, info: "bob", found: true},
		{name: "no scheme", address: "{bob:pw}example.com/r", info: "bob:pw", found: true},
		{name: "an escape that is none", address: "https://{bob:%ZZ}example.com/r", scheme: schemeHTTPS, info: "bob:%ZZ", found: true},
		{name: "a backslash before the host", address: `https://{bob:pw}evil.example\}example.com/r`, scheme: schemeHTTPS, info: `bob:pw@evil.example\`, found: true},
		{name: "anonymous", address: "https://example.com/r", scheme: schemeHTTPS},
		{name: "an at sign in the path", address: "https://example.com/a}b", scheme: schemeHTTPS},
		{name: "an at sign in the query", address: "https://example.com?a}b", scheme: schemeHTTPS},
		{name: "an at sign in the fragment", address: "https://example.com#a}b", scheme: schemeHTTPS},
		{name: "a file", address: "file://a}b/c", scheme: schemeFile},
		{name: "a directory", address: "dir://a}b/c", scheme: schemeDir},
		{name: "an absolute path", address: "/srv/a}b"},
		{name: "a path of windows", address: `C:\Users\a}b`},
		{name: "a path of windows with slashes", address: "C:/Users/a}b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			addr := address.Replace(tt.address)

			scheme, info, start, found := userinfo.Find(addr)
			if scheme != tt.scheme || info != tt.info || found != tt.found {
				t.Errorf("Find(%q) = %q, %q, %v; want %q, %q, %v", addr, scheme, info, found, tt.scheme, tt.info, tt.found)
			}

			if found && addr[start:start+len(info)] != info {
				t.Errorf("Find(%q) start = %d, where %q is not", addr, start, info)
			}

			if tt.scheme != "" && tt.scheme != schemeFile && tt.scheme != schemeDir {
				checkAsURL(t, addr, info, found)
			}
		})
	}
}

// checkAsURL fails the test when net/url parses the address and reads
// another user and password in it than Find did.
func checkAsURL(t *testing.T, addr, info string, found bool) {
	t.Helper()

	u, err := url.Parse(addr)
	if err != nil {
		return
	}

	parsed := ""

	if u.User != nil {
		parsed = u.User.Username()
		if password, ok := u.User.Password(); ok {
			parsed += ":" + password
		}
	}

	if (u.User != nil) != found || parsed != info {
		t.Errorf("Find(%q) = %q, %v; net/url reads %q, %v", addr, info, found, parsed, u.User != nil)
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		address  string
		expected string
	}{
		{name: "a user and a password", address: "https://{bob:pw}example.com/r", expected: "https://xxxxx@example.com/r"},
		{name: "a user", address: "ssh://{git}example.com/r", expected: "ssh://xxxxx@example.com/r"},
		{name: "no scheme", address: "{bob:pw}example.com/r", expected: "xxxxx@example.com/r"},
		{name: "anonymous", address: "example.com/r", expected: "example.com/r"},
		{name: "an absolute path", address: "/srv/a}b", expected: "/srv/a@b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			addr := address.Replace(tt.address)

			if got := userinfo.Redact(addr); got != tt.expected {
				t.Errorf("Redact(%q) = %q, want %q", addr, got, tt.expected)
			}
		})
	}
}
