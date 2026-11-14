// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestCDCPolicy_Parse(t *testing.T) {
	ci.Parallel(t)

	p, err := Parse(`
cdc {
  receive = ["norca.eu.games.Job", "norca.eu.games.ConsulKV.>"]
  leaf    = true
}`)
	must.NoError(t, err)
	must.NotNil(t, p.CDC)
	must.Eq(t, []string{"norca.eu.games.Job", "norca.eu.games.ConsulKV.>"}, p.CDC.Receive)
	must.True(t, p.CDC.Leaf)
	must.False(t, p.IsEmpty())

	tests := []struct {
		name     string
		rules    string
		expected string
	}{
		{name: "no subjects", rules: `cdc { leaf = true }`, expected: "receive"},
		{name: "inbox", rules: `cdc { receive = ["_INBOX.>"] }`, expected: "_INBOX"},
		{name: "leading wildcard", rules: `cdc { receive = [">"] }`, expected: "wildcard"},
		{name: "system account", rules: `cdc { receive = ["$SYS.>"] }`, expected: "$SYS"},
		{name: "unknown key", rules: `cdc { receive = ["a.b"] subscribe = ["c"] }`, expected: `unknown key "subscribe" in the cdc rule`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.rules)
			must.ErrorContains(t, err, "Invalid cdc policy")
			must.ErrorContains(t, err, tt.expected)
		})
	}
}

func TestACL_CDCGrant(t *testing.T) {
	ci.Parallel(t)

	// ACLs disabled and management tokens receive everything, also as a leaf.
	var disabled *ACL
	must.True(t, disabled.CDCGrant().All)
	must.True(t, ManagementACL.CDCGrant().Leaf)

	p1, err := Parse(`cdc { receive = ["norca.eu.games.Job"] }`)
	must.NoError(t, err)
	p2, err := Parse(`
cdc {
  receive = ["norca.eu.games.Allocation", "norca.eu.games.Job"]
  leaf    = true
}`)
	must.NoError(t, err)
	p3, err := Parse(`namespace "default" { policy = "read" }`)
	must.NoError(t, err)

	a, err := NewACL(false, []*Policy{p1, p3})
	must.NoError(t, err)
	g := a.CDCGrant()
	must.False(t, g.All)
	must.False(t, g.Leaf)
	must.Eq(t, []string{"norca.eu.games.Job"}, g.Receive)

	// The rules of the policies are joined; one with leaf allows the leaf.
	a, err = NewACL(false, []*Policy{p1, p2})
	must.NoError(t, err)
	g = a.CDCGrant()
	must.True(t, g.Leaf)
	must.Eq(t, []string{"norca.eu.games.Allocation", "norca.eu.games.Job"}, g.Receive)

	// A token without a cdc rule receives nothing.
	a, err = NewACL(false, []*Policy{p3})
	must.NoError(t, err)
	must.True(t, a.CDCGrant().Empty())
}
