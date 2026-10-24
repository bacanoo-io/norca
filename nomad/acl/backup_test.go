// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestBackupACL(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		rules string
		read  bool
		write bool
	}{
		{`backup { policy = "read" }`, true, false},
		{`backup { policy = "write" }`, true, true},
		{`backup { policy = "deny" }`, false, false},
		{`operator { policy = "write" }`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.rules, func(t *testing.T) {
			p, err := Parse(tc.rules)
			must.NoError(t, err)
			a, err := NewACL(false, []*Policy{p})
			must.NoError(t, err)
			must.Eq(t, tc.read, a.AllowBackupRead())
			must.Eq(t, tc.write, a.AllowBackupWrite())
		})
	}

	// Deny takes precedence when policies are merged.
	p1, err := Parse(`backup { policy = "write" }`)
	must.NoError(t, err)
	p2, err := Parse(`backup { policy = "deny" }`)
	must.NoError(t, err)
	a, err := NewACL(false, []*Policy{p1, p2})
	must.NoError(t, err)
	must.False(t, a.AllowBackupRead())

	// Invalid policy value.
	_, err = Parse(`backup { policy = "scale" }`)
	must.ErrorContains(t, err, "Invalid backup policy")

	// Management and disabled ACLs allow everything.
	must.True(t, ManagementACL.AllowBackupWrite())
	var nilACL *ACL
	must.True(t, nilACL.AllowBackupWrite())
}
