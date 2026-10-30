// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestBusPolicy_Parse(t *testing.T) {
	ci.Parallel(t)

	p, err := Parse(`
bus {
  subscribe = ["nomad.*.>", "nomad.global.Job"]
  publish   = ["nomad.packs.pushed"]
  leafnode  = true
}`)
	must.NoError(t, err)
	must.NotNil(t, p.Bus)
	must.Eq(t, []string{"nomad.*.>", "nomad.global.Job"}, p.Bus.Subscribe)
	must.Eq(t, []string{"nomad.packs.pushed"}, p.Bus.Publish)
	must.True(t, p.Bus.Leafnode)
	must.False(t, p.IsEmpty())

	for _, bad := range []string{
		`bus { subscribe = [""] }`,
		`bus { subscribe = ["a..b"] }`,
		`bus { subscribe = ["a.>.b"] }`,
		`bus { publish = ["a.b*"] }`,
		`bus { publish = ["a b"] }`,
		`bus { subscribe = ["$SYS.>"] }`,
		`bus { subscribe = ["$SYS"] }`,
	} {
		_, err := Parse(bad)
		must.ErrorContains(t, err, "Invalid bus policy", must.Sprint(bad))
	}
}

func TestACL_BusGrant(t *testing.T) {
	ci.Parallel(t)

	// ACLs disabled and management tokens may use everything.
	var disabled *ACL
	must.True(t, disabled.BusGrant().All)
	must.True(t, ManagementACL.BusGrant().All)

	p1, err := Parse(`bus { subscribe = ["nomad.*.>"] }`)
	must.NoError(t, err)
	p2, err := Parse(`
bus {
  subscribe = ["nomad.*.>", "app.>"]
  publish   = ["nomad.packs.pushed"]
}`)
	must.NoError(t, err)
	p3, err := Parse(`namespace "default" { policy = "read" }`)
	must.NoError(t, err)

	a, err := NewACL(false, []*Policy{p1, p2, p3})
	must.NoError(t, err)
	g := a.BusGrant()
	must.False(t, g.All)
	must.False(t, g.Leafnode)
	must.Eq(t, []string{"app.>", "nomad.*.>"}, g.Subscribe)
	must.Eq(t, []string{"nomad.packs.pushed"}, g.Publish)
	must.False(t, g.Empty())

	// A token without a bus rule gets nothing.
	a, err = NewACL(false, []*Policy{p3})
	must.NoError(t, err)
	must.True(t, a.BusGrant().Empty())
}
