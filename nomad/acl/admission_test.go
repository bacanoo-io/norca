// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestAdmissionACL(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		rules string
		read  bool
		write bool
	}{
		{`admission { policy = "read" }`, true, false},
		{`admission { policy = "write" }`, true, true},
		{`admission { policy = "deny" }`, false, false},
		{`operator { policy = "write" }`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.rules, func(t *testing.T) {
			p, err := Parse(tc.rules)
			must.NoError(t, err)
			a, err := NewACL(false, []*Policy{p})
			must.NoError(t, err)
			must.Eq(t, tc.read, a.AllowAdmissionRead())
			must.Eq(t, tc.write, a.AllowAdmissionWrite())
		})
	}

	// Deny takes precedence when policies are merged.
	p1, err := Parse(`admission { policy = "write" }`)
	must.NoError(t, err)
	p2, err := Parse(`admission { policy = "deny" }`)
	must.NoError(t, err)
	a, err := NewACL(false, []*Policy{p1, p2})
	must.NoError(t, err)
	must.False(t, a.AllowAdmissionRead())

	// Invalid policy value.
	_, err = Parse(`admission { policy = "scale" }`)
	must.ErrorContains(t, err, "Invalid admission policy")

	// Management and disabled ACLs allow everything.
	must.True(t, ManagementACL.AllowAdmissionWrite())
	var nilACL *ACL
	must.True(t, nilACL.AllowAdmissionWrite())
}
