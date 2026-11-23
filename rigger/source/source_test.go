// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package source_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/source"
)

func TestParse(t *testing.T) {
	t.Parallel()

	registries := map[string]string{"example": "github.com/example/rigger-registry", "local": "file:///srv/registry"}

	tests := []struct {
		address  string
		expected source.Request
		err      error
	}{
		{address: "example/postgres", expected: source.Request{Kind: source.KindGit, Registry: "example", Repository: "github.com/example/rigger-registry", Path: "modules/postgres", TagPrefix: "postgres/v"}},
		{address: "local/nats", expected: source.Request{Kind: source.KindGit, Registry: "local", Repository: "file:///srv/registry", Path: "modules/nats", TagPrefix: "nats/v"}},
		{address: "github.com/acme/rigger-modules//nats", expected: source.Request{Kind: source.KindGit, Repository: "github.com/acme/rigger-modules", Path: "nats", TagPrefix: "nats/v"}},
		{address: "github.com/acme/redis", expected: source.Request{Kind: source.KindGit, Repository: "github.com/acme/redis", Path: ".", TagPrefix: "v"}},
		{address: "https://git.internal/rigger.git//modules/db", expected: source.Request{Kind: source.KindGit, Repository: "https://git.internal/rigger.git", Path: "modules/db", TagPrefix: "db/v"}},
		{address: "builtin/redis", expected: source.Request{Kind: source.KindBuiltin, Path: "modules/redis"}},
		{address: "dir:///opt/rigger/worker", expected: source.Request{Kind: source.KindDirectory, Repository: "/opt/rigger/worker", Path: "."}},
		{address: "oci://registry.internal/rigger/nats", expected: source.Request{Kind: source.KindOCI, Repository: "registry.internal/rigger/nats"}},
		{address: "oci://registry.internal", err: source.ErrAddress},
		{address: "norca/nats", expected: source.Request{Kind: source.KindGlobal, Namespace: "norca", Name: "nats", Path: "."}},
		{address: "Example/x", err: source.ErrAddress},
	}

	for _, tt := range tests {
		got, err := source.Parse(tt.address, registries)
		if !errors.Is(err, tt.err) {
			t.Fatalf("Parse(%q) error = %v, want %v", tt.address, err, tt.err)
		}

		tt.expected.Source = tt.address
		if tt.err == nil && got != tt.expected {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.address, got, tt.expected)
		}
	}
}

// secretAddress writes a test address with credentials: {bob:PW} stands
// for bob:s3cret@, so that a linter reads no credentials in a URL.
var secretAddress = strings.NewReplacer("{", "", "}", "@", "PW", "s3cret")

// An address is no secret: it is written into lock files, release records
// and errors. One that carries a user or a password of http, or a password
// of ssh, is refused, and the error does not repeat what it carried.
func TestParse_Credentials(t *testing.T) {
	t.Parallel()

	registries := map[string]string{
		"private": secretAddress.Replace("https://{bob:PW}git.internal/registry"),
		"ssh":     secretAddress.Replace("ssh://{deploy}git.internal/registry"),
	}

	tests := []struct {
		name    string
		address string
		refused bool
		invalid bool
	}{
		{name: "https, a user and a password", address: "https://{bob:PW}git.internal/rigger.git//modules/db", refused: true},
		{name: "https, a token as the user", address: "https://{PW}github.com/acme/redis", refused: true},
		{name: "http, a password only", address: "http://{:PW}git.internal/rigger.git", refused: true},
		{name: "a host and a path", address: "{bob:PW}github.com/acme/redis", refused: true},
		{name: "ssh, a password", address: "ssh://{git:PW}git.internal/org/repo", refused: true},
		{name: "oci", address: "oci://{bob:PW}registry.internal/rigger/nats", refused: true},
		{name: "a registry of the stack", address: "private/web", refused: true},
		{name: "no source at all", address: "{bob:PW}localhost/repo", invalid: true},
		{name: "ssh, a user", address: "ssh://{deploy}git.internal/org/repo"},
		{name: "scp", address: "{git}git.internal:org/repo"},
		{name: "a registry over ssh", address: "ssh/web"},
		{name: "a path with an at sign", address: "github.com/acme/redis//modules/a}b"},
		{name: "a file with an at sign", address: "file:///srv/a}b/registry"},
		{name: "a directory with an at sign", address: "/srv/a}b/registry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			address := secretAddress.Replace(tt.address)

			_, err := source.Parse(address, registries)

			if got := errors.Is(err, source.ErrCredentials); got != tt.refused {
				t.Errorf("Parse(%q) refused for its credentials = %v, want %v: %v", address, got, tt.refused, err)
			}

			if (tt.refused || tt.invalid) && !errors.Is(err, source.ErrAddress) {
				t.Errorf("Parse(%q) = %v, want ErrAddress", address, err)
			}

			if !tt.refused && !tt.invalid && err != nil {
				t.Errorf("Parse(%q) = %v", address, err)
			}

			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("Parse(%q): the error repeats the secret: %v", address, err)
			}
		})
	}
}

// Redact takes the user and the password out of an address that is printed,
// and nothing else.
func TestRedact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		address  string
		expected string
	}{
		{name: "https, a user and a password", address: "https://{bob:PW}git.internal/r.git//m", expected: "https://xxxxx@git.internal/r.git//m"},
		{name: "https, a token as the user", address: "https://{PW}github.com/acme/redis", expected: "https://xxxxx@github.com/acme/redis"},
		{name: "an at sign in the password", address: "https://{bob:s3}cret}git.internal/r", expected: "https://xxxxx@git.internal/r"},
		{name: "a URL that does not parse", address: "https://{bob:s3%ZZcret}git.internal/r", expected: "https://xxxxx@git.internal/r"},
		{name: "a host and a path", address: "{bob:PW}github.com/acme/redis", expected: "xxxxx@github.com/acme/redis"},
		{name: "ssh, a password", address: "ssh://{git:PW}git.internal/org/repo", expected: "ssh://xxxxx@git.internal/org/repo"},
		{name: "oci", address: "oci://{bob:PW}registry.internal/nats", expected: "oci://xxxxx@registry.internal/nats"},
		{name: "ssh, a user", address: "ssh://{deploy}git.internal/org/repo", expected: "ssh://deploy@git.internal/org/repo"},
		{name: "scp", address: "{git}git.internal:org/repo", expected: "git@git.internal:org/repo"},
		{name: "https, anonymous", address: "https://git.internal/r.git", expected: "https://git.internal/r.git"},
		{name: "an at sign in the path", address: "https://git.internal/a}b", expected: "https://git.internal/a@b"},
		{name: "an at sign in the query", address: "https://git.internal?a}b", expected: "https://git.internal?a@b"},
		{name: "a file", address: "file:///srv/a}b/registry", expected: "file:///srv/a@b/registry"},
		{name: "a directory", address: "/srv/a}b/registry", expected: "/srv/a@b/registry"},
		{name: "a directory of windows", address: `C:\Users\a}b\registry`, expected: `C:\Users\a@b\registry`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			address := secretAddress.Replace(tt.address)

			if got := source.Redact(address); got != tt.expected {
				t.Errorf("Redact(%q) = %q, want %q", address, got, tt.expected)
			}
		})
	}
}
