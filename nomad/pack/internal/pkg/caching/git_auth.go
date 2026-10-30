// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package caching

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitAuth holds the credentials of a private git registry (cluster pack
// registries, nomad/nomad/pack_registry.go). They are given to one git
// command through its environment and never written into the clone, the
// cache or a URL:
//
//   - https sources: Username (default "git") and Password, a token or
//     password, sent as a basic Authorization header that git only adds to
//     requests to the source URL, without following redirects;
//   - ssh sources (ssh://, user@host:path): SSHPrivateKey and KnownHosts;
//     the host key must be in KnownHosts.
type GitAuth struct {
	Username      string
	Password      string
	SSHPrivateKey string
	KnownHosts    string

	// caFile is a CA bundle for the https source (tests).
	caFile string
}

// gitCloneTimeout bounds an authenticated clone.
const gitCloneTimeout = 10 * time.Minute

// IsSSHSource reports whether a git source is an ssh URL or the scp-like
// user@host:path form.
func IsSSHSource(source string) bool {
	if strings.HasPrefix(source, "ssh://") || strings.HasPrefix(source, "git+ssh://") {
		return true
	}
	if strings.Contains(source, "://") {
		return false
	}
	at := strings.Index(source, "@")
	colon := strings.Index(source, ":")
	return at > 0 && colon > at
}

// cloneAuthenticated clones source at ref into dst with git and auth.
func cloneAuthenticated(dst, source, ref string, latest bool, auth *GitAuth) error {
	env, cleanup, err := gitAuthEnv(source, auth)
	defer cleanup()
	if err != nil {
		return err
	}
	run := func(dir string, args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), gitCloneTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("git %s: %s", args[0], msg)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if latest {
		return run("", "clone", "--quiet", "--depth", "1", "--", source, dst)
	}
	// A branch or tag clones shallowly; a commit needs the history.
	if err := run("", "clone", "--quiet", "--depth", "1", "--branch", ref, "--", source, dst); err == nil {
		return nil
	}
	_ = os.RemoveAll(dst)
	if err := run("", "clone", "--quiet", "--", source, dst); err != nil {
		return err
	}
	return run(dst, "checkout", "--quiet", ref)
}

// gitAuthEnv returns the environment of the git commands: the process
// environment without git and ssh settings of its own, no prompts, no
// system or global configuration and no credential helpers, plus the
// credentials of auth.
func gitAuthEnv(source string, auth *GitAuth) ([]string, func(), error) {
	noop := func() {}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(k), "GIT_") || strings.EqualFold(k, "SSH_AUTH_SOCK") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	config := [][2]string{
		{"credential.helper", ""},
		{"http.followRedirects", "false"},
		{"protocol.allow", "never"},
		{"protocol.https.allow", "always"},
		{"protocol.ssh.allow", "always"},
	}

	if IsSSHSource(source) {
		if auth.SSHPrivateKey == "" || auth.KnownHosts == "" {
			return nil, noop, errors.New("an ssh source needs the items ssh_private_key and known_hosts")
		}
		dir, err := os.MkdirTemp("", "norca-git-ssh-")
		if err != nil {
			return nil, noop, err
		}
		cleanup := func() { _ = os.RemoveAll(dir) }
		key := filepath.Join(dir, "key")
		hosts := filepath.Join(dir, "known_hosts")
		keyText := strings.TrimSpace(auth.SSHPrivateKey) + "\n"
		if err := os.WriteFile(key, []byte(keyText), 0o600); err != nil {
			return nil, cleanup, err
		}
		if err := os.WriteFile(hosts, []byte(strings.TrimSpace(auth.KnownHosts)+"\n"), 0o600); err != nil {
			return nil, cleanup, err
		}
		ssh := fmt.Sprintf("ssh -F %s -i %q -o IdentitiesOnly=yes -o IdentityAgent=none -o UserKnownHostsFile=%q -o GlobalKnownHostsFile=%s -o StrictHostKeyChecking=yes -o BatchMode=yes",
			os.DevNull, filepath.ToSlash(key), filepath.ToSlash(hosts), os.DevNull)
		env = append(env, "GIT_SSH_COMMAND="+ssh)
		return withConfig(env, config), cleanup, nil
	}

	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, noop, errors.New("credentials need an https:// or ssh source")
	}
	if u.User != nil {
		return nil, noop, errors.New("the source must not carry credentials; they come from the auth secret")
	}
	if auth.Password == "" {
		return nil, noop, errors.New("an https source needs the item password or token")
	}
	user := auth.Username
	if user == "" {
		user = "git"
	}
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + auth.Password))
	// http.<url>.extraHeader applies to the URLs under the source host only.
	config = append(config, [2]string{"http." + u.Scheme + "://" + u.Host + "/.extraHeader", "Authorization: Basic " + basic})
	if auth.caFile != "" {
		config = append(config, [2]string{"http.sslCAInfo", auth.caFile})
	}
	return withConfig(env, config), noop, nil
}

func withConfig(env []string, config [][2]string) []string {
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(config)))
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}
