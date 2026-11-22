// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package credstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	registryHost = "registry.example.com"
	gitHost      = "git.example.com"
)

// envOf is an environment of the pairs given.
func envOf(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}

	return func(k string) string { return m[k] }
}

// The variables of the environment are for the one host they name, over
// https; a host they don't name gets nothing.
func TestStore_Environment(t *testing.T) {
	t.Parallel()

	env := envOf(
		"RIGGER_OCI_USERNAME", "robot", "RIGGER_OCI_PASSWORD", "oci-fixture", "RIGGER_OCI_HOST", registryHost,
		"RIGGER_GIT_PASSWORD", "git-fixture", "RIGGER_GIT_HOST", "HTTPS://Git.Example.com:443/",
	)
	s := New(filepath.Join(t.TempDir(), "credentials.json"), env)

	tests := []struct {
		name, address, user, password string
		kind                          Kind
		ok                            bool
		err                           string
	}{
		{name: "the registry", kind: OCI, address: "https://" + registryHost, user: "robot", password: "oci-fixture", ok: true},
		{name: "another registry", kind: OCI, address: "https://evil.example.net"},
		{name: "the git host", kind: Git, address: "https://git.example.com/acme/modules", password: "git-fixture", ok: true},
		{name: "the git host, its port", kind: Git, address: "https://git.example.com:443/acme/modules", password: "git-fixture", ok: true},
		{name: "the registry for git", kind: Git, address: "https://" + registryHost + "/acme/modules"},
		{name: "another git host", kind: Git, address: "https://evil.example.net/acme/modules"},
		{name: "the git host over http", kind: Git, address: "http://git.example.com/acme/modules", err: "https"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			user, password, ok, err := s.Lookup(tt.kind, tt.address)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("Lookup() = %v, want an error with %q", err, tt.err)
				}

				return
			}

			if err != nil || ok != tt.ok || user != tt.user || password != tt.password {
				t.Errorf("Lookup() = %q, %q, %v, %v; want %q, %q, %v", user, password, ok, err, tt.user, tt.password, tt.ok)
			}
		})
	}
}

// A password without the host it is for is an error that says so, not a
// password sent anywhere or dropped without a word.
func TestStore_PasswordWithoutHost(t *testing.T) {
	t.Parallel()

	s := New(filepath.Join(t.TempDir(), "credentials.json"), envOf("RIGGER_GIT_PASSWORD", "git-fixture"))

	if _, _, _, err := s.Lookup(Git, "https://"+gitHost+"/acme/modules"); err == nil || !strings.Contains(err.Error(), "RIGGER_GIT_HOST") {
		t.Errorf("Lookup() = %v, want an error naming RIGGER_GIT_HOST", err)
	}
}

// A login keeps the credentials of a host in a file only its user reads;
// they come before the environment, for both kinds; a logout forgets them.
func TestStore_Login(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "rigger", "credentials.json")
	s := New(file, envOf("RIGGER_OCI_USERNAME", "env", "RIGGER_OCI_PASSWORD", "env-fixture", "RIGGER_OCI_HOST", registryHost))

	if err := s.Login("https://"+registryHost+":443", "robot", "file-fixture"); err != nil {
		t.Fatal(err)
	}

	if err := s.Login(gitHost, "", "token-fixture"); err != nil {
		t.Fatal(err)
	}

	checkMode(t, file)

	checkLookups(t, s, []lookup{
		{kind: OCI, address: "https://" + registryHost, user: "robot", password: "file-fixture"},
		{kind: Git, address: "https://" + gitHost + "/acme/modules", password: "token-fixture"},
		{kind: Git, address: "https://" + registryHost + "/acme/modules", user: "robot", password: "file-fixture"},
	})

	if gone, err := s.Logout(registryHost); err != nil || !gone {
		t.Fatalf("Logout() = %v, %v", gone, err)
	}

	// The environment again.
	checkLookups(t, s, []lookup{{kind: OCI, address: "https://" + registryHost, user: "env", password: "env-fixture"}})

	if gone, err := s.Logout(registryHost); err != nil || gone {
		t.Errorf("a second Logout() = %v, %v", gone, err)
	}

	data, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(data), "file-fixture") || !strings.Contains(string(data), "token-fixture") {
		t.Errorf("the file after the logout: %s, %v", data, err)
	}
}

// localhost is reached over http, as a registry of a test is.
func TestStore_Localhost(t *testing.T) {
	t.Parallel()

	s := New(filepath.Join(t.TempDir(), "credentials.json"), envOf("RIGGER_GIT_PASSWORD", "git-fixture", "RIGGER_GIT_HOST", "127.0.0.1:3000"))

	if _, password, ok, err := s.Lookup(Git, "http://127.0.0.1:3000/acme/modules"); err != nil || !ok || password != "git-fixture" {
		t.Errorf("Lookup() = %q, %v, %v", password, ok, err)
	}
}

// lookup is a Lookup with the credentials it finds.
type lookup struct {
	kind                    Kind
	address, user, password string
}

func checkLookups(t *testing.T, s *Store, lookups []lookup) {
	t.Helper()

	for _, l := range lookups {
		user, password, ok, err := s.Lookup(l.kind, l.address)
		if err != nil || !ok || user != l.user || password != l.password {
			t.Errorf("Lookup(%s, %s) = %q, %q, %v, %v; want %q, %q", l.kind, l.address, user, password, ok, err, l.user, l.password)
		}
	}
}

// checkMode checks that only its user reads the file (a mode Windows does
// not have).
func checkMode(t *testing.T, file string) {
	t.Helper()

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the file has the mode %v", info.Mode().Perm())
	}
}
