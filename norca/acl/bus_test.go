// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestBusPolicy_Parse(t *testing.T) {
	ci.Parallel(t)

	p, err := Parse(`
bus {
  subscribe = ["norca.*.>", "norca.global.Job"]
  publish   = ["app.orders"]
}`)
	must.NoError(t, err)
	must.NotNil(t, p.Bus)
	must.Eq(t, []string{"norca.*.>", "norca.global.Job"}, p.Bus.Subscribe)
	must.Eq(t, []string{"app.orders"}, p.Bus.Publish)
	must.False(t, p.IsEmpty())

	// The leafnode key is replaced by the cdc rule; unknown keys are refused
	// (HCL v1 would ignore them).
	_, err = Parse(`bus { leafnode = true }`)
	must.ErrorContains(t, err, "replaced by the cdc rule")
	_, err = Parse(`bus { subscribe = ["a.b"] typo = 1 }`)
	must.ErrorContains(t, err, `unknown key "typo" in the bus rule`)

	for _, bad := range []string{
		`bus { subscribe = [""] }`,
		`bus { subscribe = ["a..b"] }`,
		`bus { subscribe = ["a.>.b"] }`,
		`bus { publish = ["a.b*"] }`,
		`bus { publish = ["a b"] }`,
		`bus { subscribe = ["$SYS.>"] }`,
		`bus { subscribe = ["$SYS"] }`,
		// A rule that would reach the replies of other connections.
		`bus { subscribe = ["_INBOX.>"] }`,
		`bus { subscribe = ["_INBOX"] }`,
		`bus { subscribe = [">"] }`,
		`bus { subscribe = ["*.events.>"] }`,
		`bus { publish = [">"] }`,
		`bus { publish = ["_INBOX"] }`,
		`bus { publish = ["_INBOX.a..b"] }`,
		`bus { publish = ["_INBOX.>.b"] }`,
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

	p1, err := Parse(`bus { subscribe = ["norca.*.>"] }`)
	must.NoError(t, err)
	p2, err := Parse(`
bus {
  subscribe = ["norca.*.>", "app.>"]
  publish   = ["app.orders"]
}`)
	must.NoError(t, err)
	p3, err := Parse(`namespace "default" { policy = "read" }`)
	must.NoError(t, err)

	a, err := NewACL(false, []*Policy{p1, p2, p3})
	must.NoError(t, err)
	g := a.BusGrant()
	must.False(t, g.All)
	must.Eq(t, []string{"app.>", "norca.*.>"}, g.Subscribe)
	must.Eq(t, []string{"app.orders"}, g.Publish)
	must.False(t, g.Empty())

	// A token without a bus rule gets nothing.
	a, err = NewACL(false, []*Policy{p3})
	must.NoError(t, err)
	must.True(t, a.BusGrant().Empty())
}

// A publish list may name inboxes, to answer requests into them; a
// subscribe list never does.
func TestACL_BusPolicy_InboxPublish(t *testing.T) {
	ci.Parallel(t)
	for _, good := range []string{
		`bus { publish = ["_INBOX.>"] }`,
		`bus { publish = ["_INBOX.0b8f7d2e-1c4a-4f8e-9a6b-2d3c4e5f6a7b.>"] }`,
		`bus { publish = ["_INBOX.*.reply"] }`,
	} {
		policy, err := Parse(good)
		must.NoError(t, err, must.Sprint(good))
		must.Len(t, 1, policy.Bus.Publish)
	}
	for _, bad := range []string{
		`bus { subscribe = ["_INBOX.>"] }`,
		`bus { subscribe = ["_INBOX.0b8f7d2e-1c4a-4f8e-9a6b-2d3c4e5f6a7b.>"] }`,
	} {
		_, err := Parse(bad)
		must.ErrorContains(t, err, "can't be subscribed to", must.Sprint(bad))
	}
}
