// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestAdmissionPolicy_Validate(t *testing.T) {
	ci.Parallel(t)

	valid := func() *AdmissionPolicy {
		p := &AdmissionPolicy{
			Name:  "ok",
			Rules: []*AdmissionPolicyRule{{Expression: "true"}},
		}
		p.Canonicalize()
		return p
	}

	p := valid()
	must.Eq(t, AdmissionPolicyActionDeny, p.Action)
	must.NoError(t, p.Validate())

	p = valid()
	p.Name = "not ok"
	must.ErrorContains(t, p.Validate(), "invalid name")

	p = valid()
	p.Action = "audit"
	must.ErrorContains(t, p.Validate(), "invalid action")

	p = valid()
	p.Rules = nil
	must.ErrorContains(t, p.Validate(), "at least one rule")

	p = valid()
	p.Rules = append(p.Rules, &AdmissionPolicyRule{})
	must.ErrorContains(t, p.Validate(), "rule 1: missing expression")
}

func TestAdmissionPolicy_CopyAndMatch(t *testing.T) {
	ci.Parallel(t)

	p := &AdmissionPolicy{
		Name:  "p",
		Match: &AdmissionPolicyMatch{Namespaces: []string{"prod", "team-*"}},
		Rules: []*AdmissionPolicyRule{{Expression: "true", Message: "m"}},
	}
	c := p.Copy()
	must.Eq(t, p, c)
	c.Match.Namespaces[0] = "changed"
	c.Rules[0].Message = "changed"
	must.Eq(t, "prod", p.Match.Namespaces[0])
	must.Eq(t, "m", p.Rules[0].Message)

	must.True(t, p.MatchesNamespace("prod"))
	must.True(t, p.MatchesNamespace("team-a"))
	must.False(t, p.MatchesNamespace("default"))

	p.Match = nil
	must.True(t, p.MatchesNamespace("default"))
}

func TestNewAdmissionUser(t *testing.T) {
	ci.Parallel(t)

	u := NewAdmissionUser(&ACLToken{
		AccessorID: "a",
		Name:       "alice",
		Type:       ACLClientToken,
		Policies:   []string{"p1"},
		Roles:      []*ACLTokenRoleLink{{ID: "r", Name: "ops"}},
	}, nil)
	must.Eq(t, &AdmissionUser{AccessorID: "a", Name: "alice", Type: ACLClientToken,
		Policies: []string{"p1"}, Roles: []string{"ops"}}, u)

	u = NewAdmissionUser(nil, &IdentityClaims{Namespace: "ns", JobID: "j", TaskName: "t"})
	must.Eq(t, "workload:ns/j/t", u.Name)

	must.Eq(t, "anonymous", NewAdmissionUser(nil, nil).Type)
}
