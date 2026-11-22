// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// credentialServer records the credentials it is sent; asks says whether
// it asks for them (an OCI registry) or not (a git server, to which go-git
// sends them unasked).
type credentialServer struct {
	*httptest.Server

	mu   sync.Mutex
	seen []string
}

func newCredentialServer(t *testing.T, asks bool) *credentialServer {
	t.Helper()

	s := &credentialServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if ok {
			s.mu.Lock()
			s.seen = append(s.seen, user+":"+password)
			s.mu.Unlock()
		}

		if asks && !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)

	return s
}

func (s *credentialServer) host() string { return strings.TrimPrefix(s.URL, "http://") }

func (s *credentialServer) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return strings.Join(s.seen, " ")
}

// syncSource runs rigger stack sync of a stack with one module of source,
// which fails as no server has it; it returns standard error.
func syncSource(t *testing.T, source string) string {
	t.Helper()

	dir := writeTree(t, map[string]string{stackFile: "rigger {\n  name = \"foreign\"\n}\n\nmodule \"m\" {\n  source = \"" + source + "\"\n}\n"})

	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{cmdStack, cmdSync, dir}, &stdout, &stderr); code != exitError {
		t.Fatalf("rigger stack sync = %d, want it to fail (no server has the module)", code)
	}

	return stderr.String()
}

// setStdin gives the command input; the test is not parallel.
func setStdin(t *testing.T, input string) {
	t.Helper()

	stdin = strings.NewReader(input)

	t.Cleanup(func() { stdin = os.Stdin })
}

// noLogin is a store of credentials with nothing in it.
func noLogin(t *testing.T) string {
	t.Helper()

	file := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("RIGGER_CREDENTIALS_FILE", file)

	return file
}

// The credentials of the environment go to the host they name and to no
// other, whatever a stack, a lock file or an index names (RIG-44, RIG-46).
func TestRun_CredentialsOfTheirHost(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())
	noLogin(t)

	registry, otherRegistry := newCredentialServer(t, true), newCredentialServer(t, true)
	gitServer, otherGit := newCredentialServer(t, false), newCredentialServer(t, false)

	t.Setenv("RIGGER_OCI_USERNAME", "robot")
	t.Setenv("RIGGER_OCI_PASSWORD", "oci-fixture")
	t.Setenv("RIGGER_OCI_HOST", registry.host())
	t.Setenv("RIGGER_GIT_USERNAME", "robot")
	t.Setenv("RIGGER_GIT_PASSWORD", "git-fixture")
	t.Setenv("RIGGER_GIT_HOST", gitServer.host())

	syncSource(t, "oci://"+registry.host()+"/acme/web")
	syncSource(t, "oci://"+otherRegistry.host()+"/acme/web")
	syncSource(t, gitServer.URL+"/acme/modules//modules/web")
	syncSource(t, otherGit.URL+"/acme/modules//modules/web")

	for _, tt := range []struct {
		name string
		s    *credentialServer
		want string
	}{
		{name: "the registry", s: registry, want: "robot:oci-fixture"},
		{name: "another registry", s: otherRegistry},
		{name: "the git server", s: gitServer, want: "robot:git-fixture"},
		{name: "another git server", s: otherGit},
	} {
		if got := tt.s.got(); (tt.want == "") != (got == "") || (tt.want != "" && !strings.HasPrefix(got, tt.want)) {
			t.Errorf("%s got %q, want %q", tt.name, got, tt.want)
		}
	}

	// A password without its host goes nowhere and says why.
	t.Setenv("RIGGER_GIT_HOST", "")

	if stderr := syncSource(t, gitServer.URL+"/acme/other//modules/web"); !strings.Contains(stderr, "RIGGER_GIT_HOST") {
		t.Errorf("a password without its host: %q", stderr)
	}
}

// rigger login keeps the credentials of a host, which then go to it;
// rigger logout forgets them.
func TestRun_Login(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())
	file := noLogin(t)

	for _, v := range []string{"RIGGER_OCI_PASSWORD", "RIGGER_GIT_PASSWORD"} {
		t.Setenv(v, "")
	}

	registry, other := newCredentialServer(t, true), newCredentialServer(t, true)
	setStdin(t, "file-fixture\n")

	riggerRun(t, exitOK, []string{"logged in to " + registry.host()}, "login", registry.host(), "-username", "robot")

	if data, err := os.ReadFile(file); err != nil || !strings.Contains(string(data), registry.host()) {
		t.Fatalf("the file of the credentials: %s, %v", data, err)
	}

	syncSource(t, "oci://"+registry.host()+"/acme/web")
	syncSource(t, "oci://"+other.host()+"/acme/web")

	if got := registry.got(); !strings.HasPrefix(got, "robot:file-fixture") {
		t.Errorf("the registry got %q", got)
	}

	if got := other.got(); got != "" {
		t.Errorf("another registry got %q", got)
	}

	riggerRun(t, exitOK, []string{"logged out of " + registry.host()}, "logout", registry.host())
	riggerRunErr(t, "has no credentials", "logout", registry.host())
	riggerRunErr(t, "no password", "login", other.host())
}
