// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestSecretPolicies_Parse(t *testing.T) {
	ci.Parallel(t)

	p, err := Parse(`namespace "prod" {
	  capabilities = ["list-secrets"]
	  secret "db/*" { capabilities = ["read-secret", "write-secret"] }
	  secret "db/root" { capabilities = ["deny"] }
	}`)
	must.NoError(t, err)
	must.Len(t, 2, p.Namespaces[0].Secrets)
	must.Eq(t, "db/*", p.Namespaces[0].Secrets[0].PathSpec)

	for name, rules := range map[string]string{
		"bad capability": `namespace "prod" { secret "a" { capabilities = ["read"] } }`,
		"no capability":  `namespace "prod" { secret "a" { } }`,
		"no path":        `namespace "prod" { secret { capabilities = ["read-secret"] } }`,
	} {
		_, err := Parse(rules)
		must.Error(t, err, must.Sprint(name))
	}

	// The shorthands grant no secret capability.
	for _, policy := range []string{PolicyRead, PolicyWrite} {
		for _, cap := range expandNamespacePolicy(policy) {
			must.False(t, isSecretCapability(cap))
		}
	}
}

func TestSecretPolicies_Allow(t *testing.T) {
	ci.Parallel(t)

	compile := func(rules string) *ACL {
		p, err := Parse(rules)
		must.NoError(t, err)
		a, err := NewACL(false, []*Policy{p})
		must.NoError(t, err)
		return a
	}

	shorthand := compile(`namespace "prod" { policy = "write" }`)
	must.False(t, shorthand.AllowSecretOperation("prod", "db/pw", NamespaceCapabilityListSecrets))
	must.False(t, shorthand.AllowSecretOperation("prod", "db/pw", NamespaceCapabilityReadSecret))
	must.False(t, shorthand.AllowSecretSearch("prod"))

	a := compile(`
	namespace "prod" {
	  capabilities = ["list-secrets"]
	  secret "db/*" { capabilities = ["read-secret"] }
	  secret "db/root" { capabilities = ["deny"] }
	}
	namespace "*" {
	  secret "shared/*" { capabilities = ["read-secret", "write-secret"] }
	}
	namespace "dev" {
	  capabilities = ["deny"]
	}`)

	cases := []struct {
		ns, path, op string
		allowed      bool
	}{
		{"prod", "app/x", NamespaceCapabilityListSecrets, true},
		{"prod", "app/x", NamespaceCapabilityReadSecret, false},
		{"prod", "db/pw", NamespaceCapabilityReadSecret, true},
		{"prod", "db/pw", NamespaceCapabilityWriteSecret, false},
		{"prod", "db/root", NamespaceCapabilityListSecrets, false},
		{"prod", "db/root", NamespaceCapabilityReadSecret, false},
		{"staging", "shared/token", NamespaceCapabilityReadSecret, true},
		{"staging", "shared/token", NamespaceCapabilityListSecrets, true},
		{"staging", "shared/token", NamespaceCapabilityDestroySecret, false},
		{"staging", "other", NamespaceCapabilityListSecrets, false},
		{"dev", "shared/token", NamespaceCapabilityReadSecret, false},
	}
	for _, c := range cases {
		must.Eq(t, c.allowed, a.AllowSecretOperation(c.ns, c.path, c.op),
			must.Sprintf("%s %s %s", c.ns, c.path, c.op))
	}
	must.True(t, a.AllowSecretSearch("prod"))
	must.True(t, a.AllowSecretSearch("staging"))
	must.True(t, a.AllowSecretSearch("*"))

	none := compile(`namespace "prod" { policy = "read" }`)
	must.False(t, none.AllowSecretSearch("*"))

	var disabled *ACL
	must.True(t, disabled.AllowSecretOperation("prod", "x", NamespaceCapabilityDestroySecret))
	must.True(t, ManagementACL.AllowSecretOperation("prod", "x", NamespaceCapabilityDestroySecret))
}
