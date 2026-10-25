// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestNetworkPolicyACL(t *testing.T) {
	ci.Parallel(t)

	compile := func(t *testing.T, rules string) *ACL {
		t.Helper()
		policy, err := Parse(rules)
		must.NoError(t, err)
		aclObj, err := NewACL(false, []*Policy{policy})
		must.NoError(t, err)
		return aclObj
	}

	writer := compile(t, `namespace "prod" { network_policy { policy = "write" } }`)
	must.True(t, writer.AllowNetworkPolicyRead("prod"))
	must.True(t, writer.AllowNetworkPolicyWrite("prod"))
	must.False(t, writer.AllowNetworkPolicyRead("dev"))
	// The network_policy rule grants nothing else in the namespace.
	must.False(t, writer.AllowNsOp("prod", NamespaceCapabilityReadJob))

	reader := compile(t, `namespace "prod" { network_policy { policy = "read" } }`)
	must.True(t, reader.AllowNetworkPolicyRead("prod"))
	must.False(t, reader.AllowNetworkPolicyWrite("prod"))

	// Namespace read and write include read-network-policy only.
	nsWrite := compile(t, `namespace "*" { policy = "write" }`)
	must.True(t, nsWrite.AllowNetworkPolicyRead("prod"))
	must.False(t, nsWrite.AllowNetworkPolicyWrite("prod"))

	explicit := compile(t, `namespace "prod" { capabilities = ["write-network-policy"] }`)
	must.True(t, explicit.AllowNetworkPolicyWrite("prod"))

	var nilACL *ACL
	must.True(t, nilACL.AllowNetworkPolicyWrite("prod"))

	_, err := Parse(`namespace "prod" { network_policy { policy = "deny" } }`)
	must.ErrorContains(t, err, "Invalid network_policy policy")
}
