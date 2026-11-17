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

// Among equally specific globs a deny wins, whatever the names of the globs
// are (review finding ACL-2).
func TestFindClosestMatchingGlob_TieDeny(t *testing.T) {
	ci.Parallel(t)

	// The deny is first in the order of the tree.
	a := reviewACL(t,
		`namespace "*-prod" { policy = "deny" }`,
		`namespace "team-*" { policy = "write" }`,
	)
	must.False(t, a.AllowNamespaceOperation("team-prod", NamespaceCapabilitySubmitJob))
	must.False(t, a.AllowNamespace("team-prod"))
	must.True(t, a.AllowNamespaceOperation("team-dev", NamespaceCapabilitySubmitJob))

	// The deny is last in the order of the tree.
	a = reviewACL(t,
		`namespace "a-*" { policy = "deny" }`,
		`namespace "*-b" { policy = "write" }`,
	)
	must.False(t, a.AllowNamespaceOperation("a-b", NamespaceCapabilitySubmitJob))
	must.True(t, a.AllowNamespaceOperation("c-b", NamespaceCapabilitySubmitJob))

	// A more specific glob still wins over a less specific deny.
	a = reviewACL(t,
		`namespace "*" { policy = "deny" }`,
		`namespace "team-*" { policy = "write" }`,
	)
	must.True(t, a.AllowNamespaceOperation("team-prod", NamespaceCapabilitySubmitJob))

	// Without a deny the choice among equally specific globs is the same in
	// every run: the last one in the order of the tree.
	a = reviewACL(t,
		`namespace "*-prod" { policy = "read" }`,
		`namespace "team-*" { policy = "write" }`,
	)
	for i := 0; i < 10; i++ {
		must.True(t, a.AllowNamespaceOperation("team-prod", NamespaceCapabilitySubmitJob))
	}

	// The same holds for the other rules matched by globs.
	a = reviewACL(t,
		`host_volume "*-prod" { policy = "deny" }`,
		`host_volume "team-*" { policy = "write" }`,
		`node_pool "*-prod" { policy = "deny" }`,
		`node_pool "team-*" { policy = "write" }`,
		`namespace "ns" { variables {
		   path "*-prod" { capabilities = ["deny"] }
		   path "team-*" { capabilities = ["read"] }
		 } }`,
		`namespace "ns" {
		   object "g.io/*K" { policy = "deny" }
		   object "g.io/K*" { policy = "write" }
		   secret "*-prod" { capabilities = ["deny"] }
		   secret "team-*" { capabilities = ["read-secret"] }
		 }`,
	)
	must.False(t, a.AllowHostVolumeOperation("team-prod", HostVolumeCapabilityMountReadOnly))
	must.False(t, a.AllowNodePoolOperation("team-prod", NodePoolCapabilityRead))
	must.False(t, a.AllowVariableOperation("ns", "team-prod", VariablesCapabilityRead, nil))
	must.True(t, a.AllowVariableOperation("ns", "team-dev", VariablesCapabilityRead, nil))
	must.False(t, a.AllowObjectOperation("ns", "g.io/K", ObjectCapabilityRead))
	must.False(t, a.AllowSecretOperation("ns", "team-prod", NamespaceCapabilityReadSecret))
	must.True(t, a.AllowSecretOperation("ns", "team-dev", NamespaceCapabilityReadSecret))
}

// A namespace deny denies the objects of the namespace too, whatever object
// rule another policy has there (review finding ACL-1).
func TestObjects_NamespaceDeny(t *testing.T) {
	ci.Parallel(t)

	a := reviewACL(t,
		`namespace "prod" { policy = "deny" }`,
		`namespace "prod" { object "g.io/K" { policy = "write" } }`,
	)
	must.False(t, a.AllowNamespace("prod"))
	must.False(t, a.AllowObjectOperation("prod", "g.io/K", ObjectCapabilityWrite))
	must.False(t, a.AllowObjectOperation("prod", "g.io/K", ObjectCapabilityRead))
	must.False(t, a.AllowObjectOperationInAnyNamespace("g.io/K", ObjectCapabilityWrite))
	must.False(t, a.AllowAnyObjectOperation(ObjectCapabilityWrite))
	must.False(t, a.AllowObjectKindRead("g.io/K"))

	// A denied glob namespace denies the object rules of the same glob, and
	// of the namespaces it is the closest match of.
	a = reviewACL(t,
		`namespace "team-*" { policy = "deny" }`,
		`namespace "team-*" { object "g.io/K" { policy = "write" } }`,
		`namespace "*" { object "g.io/K" { policy = "read" } }`,
	)
	must.False(t, a.AllowObjectOperation("team-a", "g.io/K", ObjectCapabilityRead))
	must.True(t, a.AllowObjectOperation("other", "g.io/K", ObjectCapabilityRead))
	must.True(t, a.AllowObjectOperation(ObjectClusterNamespace, "g.io/K", ObjectCapabilityRead))
	must.False(t, a.AllowObjectOperationInAnyNamespace("g.io/K", ObjectCapabilityWrite))
	must.True(t, a.AllowObjectOperationInAnyNamespace("g.io/K", ObjectCapabilityRead))
	must.False(t, a.AllowAnyObjectOperation(ObjectCapabilityWrite))
	must.True(t, a.AllowAnyObjectOperation(ObjectCapabilityRead))

	// The rules of another namespace are untouched, and a namespace rule
	// without capabilities is no deny.
	a = reviewACL(t,
		`namespace "prod" { policy = "deny" }`,
		`namespace "dev" { object "g.io/K" { policy = "write" } }`,
	)
	must.True(t, a.AllowObjectOperation("dev", "g.io/K", ObjectCapabilityWrite))
	must.True(t, a.AllowObjectOperationInAnyNamespace("g.io/K", ObjectCapabilityWrite))
	must.True(t, a.AllowAnyObjectOperation(ObjectCapabilityWrite))
}
