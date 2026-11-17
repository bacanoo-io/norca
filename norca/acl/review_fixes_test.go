// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// reviewACL compiles the policies given as HCL rules into an ACL object.
func reviewACL(t *testing.T, rules ...string) *ACL {
	t.Helper()
	policies := make([]*Policy, 0, len(rules))
	for _, r := range rules {
		p, err := Parse(r)
		must.NoError(t, err)
		policies = append(policies, p)
	}
	a, err := NewACL(false, policies)
	must.NoError(t, err)
	return a
}

// A nil ACL object means that ACLs are disabled: every check allows, none
// panics (review finding ACL-6).
func TestACL_NilAllows(t *testing.T) {
	ci.Parallel(t)

	var a *ACL
	must.True(t, a.AllowHostVolumeOperation("x", HostVolumeCapabilityMountReadOnly))
	must.True(t, a.AllowHostVolume("x"))
	must.True(t, a.AllowVariableOperation("ns", "x", VariablesCapabilityRead, nil))
	must.True(t, a.AllowVariableSearch("ns"))
	must.True(t, a.AllowAgentRead())
	must.True(t, a.AllowAgentWrite())
	must.True(t, a.AllowNodeWrite())
	must.True(t, a.AllowOperatorRead())
	must.True(t, a.AllowOperatorWrite())
	must.True(t, a.IsManagement())

	// The checks that handled nil before.
	must.True(t, a.AllowNsOp("ns", NamespaceCapabilitySubmitJob))
	must.True(t, a.AllowNamespace("ns"))
	must.True(t, a.AllowNodePoolOperation("x", NodePoolCapabilityRead))
	must.True(t, a.AllowNodePool("x"))
	must.True(t, a.AllowNodePoolSearch())
	must.True(t, a.AllowNodeRead())
	must.True(t, a.AllowQuotaRead())
	must.True(t, a.AllowQuotaWrite())
	must.True(t, a.AllowPluginRead())
	must.True(t, a.AllowPluginList())
	must.True(t, a.AllowObjectKindRead("g.io/K"))
	must.True(t, a.AllowObjectKindWrite())
}

// The secret search check counts a glob namespace rule only if the glob
// matches the namespace (review finding ACL-5).
func TestAllowSecretSearch_GlobNamespace(t *testing.T) {
	ci.Parallel(t)

	a := reviewACL(t, `namespace "dev-*" { secret "x" { capabilities = ["list-secrets"] } }`)
	must.False(t, a.AllowSecretSearch("prod"))
	must.True(t, a.AllowSecretSearch("dev-1"))
	must.True(t, a.AllowSecretSearch("*"))

	a = reviewACL(t, `namespace "prod" { secret "x/*" { capabilities = ["list-secrets"] } }`)
	must.True(t, a.AllowSecretSearch("prod"))
	must.False(t, a.AllowSecretSearch("dev"))
}

// A deny of another secret path of any namespace does not refuse the grant
// of a pattern it cannot match (review finding ACL-4).
func TestAllowSecretGrant_NamespaceSeparator(t *testing.T) {
	ci.Parallel(t)

	must.False(t, secretKeysIntersect("*\x00legacy", "prod\x00app/*"))
	must.True(t, secretKeysIntersect("*\x00app/x*", "prod\x00app/*"))
	must.False(t, secretKeysIntersect("dev-*\x00app/*", "prod\x00app/*"))

	a := reviewACL(t,
		`namespace "*" { secret "legacy" { capabilities = ["deny"] } }`,
		`namespace "prod" { secret "app/*" { capabilities = ["read-secret"] } }`,
	)
	must.True(t, a.AllowSecretOperation("prod", "app/x", NamespaceCapabilityReadSecret))
	must.True(t, a.AllowSecretGrant("prod", "app/*", NamespaceCapabilityReadSecret))

	// A deny that can match a path of the pattern still refuses it.
	a = reviewACL(t,
		`namespace "*" { secret "app/legacy" { capabilities = ["deny"] } }`,
		`namespace "prod" { secret "app/*" { capabilities = ["read-secret"] } }`,
	)
	must.False(t, a.AllowSecretGrant("prod", "app/*", NamespaceCapabilityReadSecret))
}

// The variable search check matches the namespace as a whole, and the glob
// namespaces of the rules as globs (review finding ACL-3).
func TestAllowVariableSearch_Namespace(t *testing.T) {
	ci.Parallel(t)

	a := reviewACL(t,
		`namespace "production" { variables { path "*" { capabilities = ["read"] } } }`,
		`namespace "staging" { variables { path "exact" { capabilities = ["read"] } } }`,
	)
	must.False(t, a.AllowVariableSearch("prod"))
	must.True(t, a.AllowVariableSearch("production"))
	must.False(t, a.AllowVariableSearch("stag"))
	must.True(t, a.AllowVariableSearch("staging"))
	must.True(t, a.AllowVariableSearch("*"))

	a = reviewACL(t, `namespace "*" { variables { path "*" { capabilities = ["read"] } } }`)
	must.True(t, a.AllowVariableSearch("prod"))
	must.True(t, a.AllowVariableOperation("prod", "x", VariablesCapabilityRead, nil))

	a = reviewACL(t, `namespace "dev-*" { variables { path "exact" { capabilities = ["read"] } } }`)
	must.True(t, a.AllowVariableSearch("dev-1"))
	must.False(t, a.AllowVariableSearch("prod"))
	must.False(t, a.AllowVariableSearch("dev"))
}
