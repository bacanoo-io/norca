// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// Only a connection that could not be made is a network that is not there:
// an answer that refuses, a connection that broke and a request that ended
// are not.
func TestUnreachable(t *testing.T) {
	t.Parallel()

	_, refused := (&net.Dialer{}).DialContext(t.Context(), "tcp", "127.0.0.1:1")
	if refused == nil {
		t.Fatal("something listens on 127.0.0.1:1")
	}

	dial := &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "registry.invalid", IsNotFound: true}}

	ended, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context //nolint:containedctx // the context of a case
		err      error
		expected bool
	}{
		{name: "nothing listens", err: refused, expected: true},
		{name: "no such host", err: dial, expected: true},
		{name: "as go-git returns it", err: fmt.Errorf("git clone x: %w", &url.Error{Op: "Get", URL: "https://registry.invalid/x", Err: dial}), expected: true},
		{name: "a proxy that can't be reached", err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: dial}, expected: true},
		{name: "no ssh agent", err: fmt.Errorf("the ssh agent for x: %w: %w", errNoSSHAgent, errors.New("SSH_AUTH_SOCK")), expected: true},
		{name: "no error"},
		{name: "authentication required", err: fmt.Errorf("git clone x: %w", transport.ErrAuthenticationRequired)},
		{name: "authorization failed", err: transport.ErrAuthorizationFailed},
		{name: "no such repository", err: transport.ErrRepositoryNotFound},
		{name: "a connection that broke", err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}},
		{name: "a certificate that is not trusted", err: &url.Error{Op: "Get", URL: "https://x", Err: x509.UnknownAuthorityError{}}},
		{name: "a request that ended", err: &url.Error{Op: "Get", URL: "https://x", Err: context.Canceled}},
		{name: "a deadline that passed", err: fmt.Errorf("waiting: %w", context.DeadlineExceeded)},
		{name: "a dial of a request that ended", ctx: ended, err: dial},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := tt.ctx
			if ctx == nil {
				ctx = t.Context()
			}

			if got := unreachable(ctx, tt.err); got != tt.expected {
				t.Errorf("unreachable(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}

// A machine without an ssh agent does not reach a repository it reads with
// the agent: that is the error of sshAuth, apart from a key that is none.
func TestSSHAuth_NoAgent(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	_, err := sshAuth("git@example.com:org/repo", "git", &Credentials{SSHAgent: true})
	if err == nil {
		t.Skip("this machine has an ssh agent that needs no SSH_AUTH_SOCK")
	}

	if !errors.Is(err, errNoSSHAgent) || !unreachable(t.Context(), err) {
		t.Errorf("sshAuth() without an agent = %v, want errNoSSHAgent", err)
	}

	_, err = sshAuth("git@example.com:org/repo", "git", &Credentials{SSHKey: []byte("not a key")})
	if err == nil || unreachable(t.Context(), err) {
		t.Errorf("sshAuth() with a key that is none = %v, want an error that is no missing network", err)
	}
}
