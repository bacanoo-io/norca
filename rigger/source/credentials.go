// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package source

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca/rigger/internal/userinfo"
)

// An address is not kept secret: it is written into the lock file and the
// record of a release, it names a directory of the cache, and plans and
// errors print it. So it carries no credentials. Those of a repository are
// given apart, by its host (fetch.Fetcher.SetCredentials, rigger login), and
// go to that origin only; go-git takes a user and a password of a URL with
// it over a redirect, to where the redirect leads.

// ErrCredentials is the error of an address that carries credentials: a
// user or a password of http, a password of ssh. It wraps ErrAddress.
var ErrCredentials = errors.New("credentials in an address")

// sshUser reports whether what an address has before the host is the user
// of ssh and no more: ssh needs it, and it is no secret.
func sshUser(address, scheme, info string) bool {
	return IsSCPAddress(address) || scheme == "ssh" && !strings.Contains(info, ":")
}

// CheckRepository refuses the address of a git or an OCI repository that
// carries credentials (ErrCredentials). The user of an ssh address
// (ssh://user@host/path, user@host:path) is none.
func CheckRepository(repository string) error {
	scheme, info, _, ok := userinfo.Find(repository)
	if !ok || sshUser(repository, scheme, info) {
		return nil
	}

	return fmt.Errorf("%w: %s: %w: an address is not kept secret, the credentials of its host are given apart (rigger login, the credentials of a registry)",
		ErrAddress, userinfo.Redact(repository), ErrCredentials)
}

// Redact returns an address as it may be printed: without the user and the
// password that CheckRepository refuses.
func Redact(address string) string {
	scheme, info, _, ok := userinfo.Find(address)
	if !ok || sshUser(address, scheme, info) {
		return address
	}

	return userinfo.Redact(address)
}
