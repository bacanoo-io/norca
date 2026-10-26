// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestGlobsIntersect(t *testing.T) {
	ci.Parallel(t)
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"jobs/web/*", "jobs/web/db", true},
		{"jobs/web/*", "jobs/*/db", true},
		{"jobs/web/*", "jobs/api/*", false},
		{"*", "anything", true},
		{"a*c", "ab*", true},
		{"a*c", "b*", false},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"*x*", "*y*", true},
		{"jobs/web", "jobs/web/*", false},
	} {
		must.Eq(t, tc.want, globsIntersect(tc.a, tc.b), must.Sprintf("%s ~ %s", tc.a, tc.b))
		must.Eq(t, tc.want, globsIntersect(tc.b, tc.a), must.Sprintf("%s ~ %s", tc.b, tc.a))
	}
}

func TestACL_AllowSecretGrant(t *testing.T) {
	ci.Parallel(t)

	newACL := func(rules string) *ACL {
		t.Helper()
		p, err := Parse(rules)
		must.NoError(t, err)
		a, err := NewACL(false, []*Policy{p})
		must.NoError(t, err)
		return a
	}
	read := NamespaceCapabilityReadSecret

	// A rule covering the pattern grants it; narrower or other paths don't.
	a := newACL(`namespace "default" { secret "jobs/web/*" { capabilities = ["read-secret"] } }`)
	must.True(t, a.AllowSecretGrant("default", "jobs/web/*", read))
	must.True(t, a.AllowSecretGrant("default", "jobs/web/db", read))
	must.False(t, a.AllowSecretGrant("default", "jobs/*", read))
	must.False(t, a.AllowSecretGrant("default", "jobs/api/*", read))
	must.False(t, a.AllowSecretGrant("prod", "jobs/web/*", read))

	// write-secret alone grants no read access.
	a = newACL(`namespace "default" { secret "jobs/*" { capabilities = ["write-secret"] } }`)
	must.False(t, a.AllowSecretGrant("default", "jobs/web/*", read))

	// A more specific rule without read-secret, or a deny, below the
	// pattern blocks the grant.
	a = newACL(`namespace "default" {
  secret "jobs/*" { capabilities = ["read-secret"] }
  secret "jobs/web/admin" { capabilities = ["list-secrets"] }
  secret "jobs/api/*" { capabilities = ["deny"] }
}`)
	must.True(t, a.AllowSecretOperation("default", "jobs/web/*", read))
	must.False(t, a.AllowSecretGrant("default", "jobs/web/*", read))
	must.True(t, a.AllowSecretGrant("default", "jobs/web/db", read))
	must.False(t, a.AllowSecretGrant("default", "jobs/*", read))
	must.True(t, a.AllowSecretGrant("default", "jobs/db/*", read))

	// A less specific rule without the capability doesn't matter.
	a = newACL(`namespace "default" {
  secret "*" { capabilities = ["list-secrets"] }
  secret "jobs/web/*" { capabilities = ["read-secret"] }
}`)
	must.True(t, a.AllowSecretGrant("default", "jobs/web/*", read))

	// Namespace wide read-secret.
	a = newACL(`namespace "default" { capabilities = ["read-secret"] }`)
	must.True(t, a.AllowSecretGrant("default", "jobs/*", read))
	a = newACL(`namespace "default" {
  capabilities = ["read-secret"]
  secret "jobs/web/root" { capabilities = ["deny"] }
}`)
	must.False(t, a.AllowSecretGrant("default", "jobs/*", read))
	must.True(t, a.AllowSecretGrant("default", "jobs/api/*", read))

	// Management and disabled ACLs grant everything.
	must.True(t, ManagementACL.AllowSecretGrant("default", "*", read))
	var none *ACL
	must.True(t, none.AllowSecretGrant("default", "*", read))
}
