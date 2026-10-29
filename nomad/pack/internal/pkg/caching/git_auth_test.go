// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package caching

import (
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestIsSSHSource(t *testing.T) {
	ci.Parallel(t)
	for src, want := range map[string]bool{
		"ssh://git@example.com/org/packs.git": true,
		"git@example.com:org/packs.git":       true,
		"https://example.com/org/packs.git":   false,
		"https://user@example.com/org/packs":  false,
		"/srv/packs":                          false,
	} {
		must.Eq(t, want, IsSSHSource(src), must.Sprint(src))
	}
}

func TestGitAuthEnv(t *testing.T) {
	ci.Parallel(t)

	env, cleanup, err := gitAuthEnv("https://git.example.com/org/packs.git", &GitAuth{Username: "bot", Password: "tok"})
	defer cleanup()
	must.NoError(t, err)
	joined := strings.Join(env, "\n")
	basic := base64.StdEncoding.EncodeToString([]byte("bot:tok"))
	must.StrContains(t, joined, "=http.https://git.example.com/.extraHeader")
	must.StrContains(t, joined, "=Authorization: Basic "+basic)
	must.StrContains(t, joined, "=http.followRedirects")
	must.StrContains(t, joined, "GIT_TERMINAL_PROMPT=0")
	must.StrNotContains(t, joined, "bot:tok", must.Sprint("the credentials only appear encoded in the header"))

	for name, tc := range map[string]struct {
		source string
		auth   GitAuth
	}{
		"plain http":     {"http://git.example.com/packs.git", GitAuth{Password: "x"}},
		"user in url":    {"https://bot:x@git.example.com/packs.git", GitAuth{Password: "x"}},
		"no password":    {"https://git.example.com/packs.git", GitAuth{Username: "bot"}},
		"no known hosts": {"git@git.example.com:org/packs.git", GitAuth{SSHPrivateKey: "key"}},
		"file":           {"file:///srv/packs", GitAuth{Password: "x"}},
	} {
		_, cleanup, err := gitAuthEnv(tc.source, &tc.auth)
		cleanup()
		must.Error(t, err, must.Sprint(name))
	}

	env, cleanup, err = gitAuthEnv("git@git.example.com:org/packs.git", &GitAuth{SSHPrivateKey: "KEY", KnownHosts: "git.example.com ssh-ed25519 AAAA"})
	defer cleanup()
	must.NoError(t, err)
	joined = strings.Join(env, "\n")
	must.StrContains(t, joined, "StrictHostKeyChecking=yes")
	must.StrContains(t, joined, "IdentityAgent=none")
}

// gitHTTPServer serves the bare repository at root/packs.git with git
// http-backend, if the basic Authorization header is want.
func gitHTTPServer(t *testing.T, root, want string, seen *atomic.Int32) *httptest.Server {
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not available")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		if _, err := os.Stat(backend + ".exe"); err != nil {
			t.Skip("git http-backend is not available")
		}
		backend += ".exe"
	}
	h := &cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="packs"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		seen.Add(1)
		h.ServeHTTP(w, r)
	}))
}

func pemCert(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func cloneWithCA(dst, source, ref string, latest bool, auth *GitAuth, caFile string) error {
	a := *auth
	a.caFile = caFile
	return cloneAuthenticated(dst, source, ref, latest, &a)
}

func TestCloneAuthenticated_HTTPS(t *testing.T) {
	ci.Parallel(t)
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		must.NoError(t, err, must.Sprint(string(out)))
	}
	root := t.TempDir()
	work := t.TempDir()
	git(work, "init", "-q", "-b", "main")
	must.NoError(t, os.MkdirAll(filepath.Join(work, "packs", "hello"), 0o755))
	must.NoError(t, os.WriteFile(filepath.Join(work, "packs", "hello", "metadata.hcl"), []byte("pack {\n  name = \"hello\"\n}\n"), 0o644))
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "packs")
	git(work, "tag", "v1")
	git(root, "clone", "-q", "--bare", work, "packs.git")

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot:s3cret"))
	var seen atomic.Int32
	srv := gitHTTPServer(t, root, want, &seen)
	defer srv.Close()
	// The test server's certificate: trust it for this test only.
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	must.NoError(t, os.WriteFile(caFile, pemCert(srv), 0o600))

	source := srv.URL + "/packs.git"
	dst := filepath.Join(t.TempDir(), "clone")
	err := cloneWithCA(dst, source, "v1", false, &GitAuth{Username: "bot", Password: "s3cret"}, caFile)
	must.NoError(t, err)
	must.Positive(t, seen.Load())
	_, err = os.Stat(filepath.Join(dst, "packs", "hello", "metadata.hcl"))
	must.NoError(t, err)

	// The credentials are not in the clone.
	cfg, err := os.ReadFile(filepath.Join(dst, ".git", "config"))
	must.NoError(t, err)
	must.StrNotContains(t, string(cfg), "s3cret")
	must.StrNotContains(t, string(cfg), "Authorization")

	// A wrong password fails without a prompt.
	err = cloneWithCA(filepath.Join(t.TempDir(), "bad"), source, "", true, &GitAuth{Username: "bot", Password: "wrong"}, caFile)
	must.Error(t, err)

	// A redirect to another host is not followed with the credentials.
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer other.Close()
	before := seen.Load()
	err = cloneWithCA(filepath.Join(t.TempDir(), "redirect"), other.URL+"/packs.git", "", true, &GitAuth{Username: "bot", Password: "s3cret"}, caFile)
	must.Error(t, err)
	must.Eq(t, before, seen.Load(), must.Sprint("the target of the redirect saw no credentials"))
}

func TestRegistrySource(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	must.Eq(t, "", RegistrySource(dir))
	must.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"),
		[]byte(`{"name":"norca","source":"https://github.com/bacanoo-io/norca-registry","ref":"v0.1.0"}`), 0o644))
	must.Eq(t, "https://github.com/bacanoo-io/norca-registry", RegistrySource(dir))
}
