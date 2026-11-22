// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/bacanoo-io/norca/rigger/source"
)

// The host and the user of the addresses of git.
const (
	sshHost = "example.com"
	gitUser = "git"
	sshPath = "org/repo"
)

// sshKey is a private key as a registry's credentials have one.
func sshKey(t *testing.T) []byte {
	t.Helper()

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	block, err := gossh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(block)
}

// An address as git writes it for ssh, user@host:path, is read over ssh as
// its user, as an ssh URL is.
func TestFetcher_SSHAddresses(t *testing.T) {
	t.Parallel()

	key := sshKey(t)
	f := New(t.TempDir())
	f.SetCredentials(func(context.Context, string) (*Credentials, error) {
		return &Credentials{SSHKey: key}, nil
	})

	tests := []struct {
		name, address, repository, path, user, host, remote string
	}{
		{name: "scp", address: "git@example.com:org/repo", user: gitUser, host: sshHost, remote: sshPath},
		{name: "scp, a user", address: "deploy@example.com:org/repo", user: "deploy", host: sshHost, remote: sshPath},
		{name: "scp, a host without a dot", address: "git@gitea:org/repo", user: gitUser, host: "gitea", remote: sshPath},
		{
			name: "scp, a module", address: "git@example.com:org/repo//modules/web", repository: "git@example.com:org/repo",
			path: "modules/web", user: gitUser, host: sshHost, remote: sshPath,
		},
		{name: "ssh, a user", address: "ssh://deploy@example.com/org/repo", user: "deploy", host: sshHost, remote: "/org/repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req, err := source.Parse(tt.address, nil)
			if err != nil {
				t.Fatal(err)
			}

			if tt.repository != "" && (req.Repository != tt.repository || req.Path != tt.path) {
				t.Fatalf("Parse() = %q, %q", req.Repository, req.Path)
			}

			u := repositoryURL(req.Repository)
			checkSSHEndpoint(t, u, tt.host, tt.remote)

			auth, err := f.auth(t.Context(), req.Repository, u)
			if err != nil {
				t.Fatal(err)
			}

			if keys, ok := auth.(*gitssh.PublicKeys); !ok || keys.User != tt.user {
				t.Errorf("auth %#v, want the key as %s", auth, tt.user)
			}
		})
	}
}

// checkSSHEndpoint checks where git reads a URL.
func checkSSHEndpoint(t *testing.T, u, host, remote string) {
	t.Helper()

	ep, err := transport.NewEndpoint(u)
	if err != nil {
		t.Fatalf("the fetcher reads %s: %v", u, err)
	}

	if ep.Protocol != "ssh" || ep.Host != host || ep.Path != remote {
		t.Fatalf("the fetcher reads %s: %s, host %s, path %s", u, ep.Protocol, ep.Host, ep.Path)
	}
}
