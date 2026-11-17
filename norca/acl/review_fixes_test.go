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
