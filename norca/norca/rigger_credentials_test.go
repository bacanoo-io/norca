// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

// The credentials that fit a source are those its transport sends.
func TestRiggerRegistry_CredentialsFit(t *testing.T) {
	ci.Parallel(t)

	key := &fetch.Credentials{SSHKey: []byte("key"), KnownHosts: []byte("hosts")}
	token := &fetch.Credentials{Password: "token"}

	for _, tt := range []struct {
		source  string
		fit, no *fetch.Credentials
	}{
		{source: "git@example.com:org/repo", fit: key, no: token},
		{source: "ssh://example.com/org/repo", fit: key, no: token},
		{source: "example.com/org/repo", fit: token, no: key},
		{source: "git@example.com/org/repo", fit: token, no: key},
	} {
		must.NoError(t, checkCredentialsFit(tt.source, tt.fit), must.Sprint(tt.source))
		must.Error(t, checkCredentialsFit(tt.source, tt.no), must.Sprint(tt.source))
	}
}
