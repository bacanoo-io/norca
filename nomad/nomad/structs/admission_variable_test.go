// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestAdmissionPolicy_ValidateVariableFields(t *testing.T) {
	ci.Parallel(t)

	valid := func() *AdmissionPolicy {
		p := &AdmissionPolicy{
			Name: "ok",
			Match: &AdmissionPolicyMatch{
				Resources:     []string{AdmissionResourceVariable},
				VariablePaths: []string{"crd/*"},
			},
			Rules: []*AdmissionPolicyRule{{Expression: "true"}},
		}
		p.Canonicalize()
		return p
	}
	must.NoError(t, valid().Validate())

	p := valid()
	p.Match.Resources = []string{"job", "node"}
	must.ErrorContains(t, p.Validate(), `invalid resource "node"`)

	p = valid()
	p.Match.Resources = nil
	must.ErrorContains(t, p.Validate(), "variable_paths requires")

	p = valid()
	p.Match.VariablePaths = []string{""}
	must.ErrorContains(t, p.Validate(), "empty variable path")

	// A schema_from policy needs no rules, but the variable resource.
	p = valid()
	p.Rules = nil
	p.SchemaFrom = AdmissionSchemaFromCRDSchema
	must.NoError(t, p.Validate())

	p = valid()
	p.SchemaFrom = "openapi"
	must.ErrorContains(t, p.Validate(), "invalid schema_from")

	p = valid()
	p.Match = nil
	p.SchemaFrom = AdmissionSchemaFromCRDSchema
	must.ErrorContains(t, p.Validate(), "schema_from requires")
}

func TestAdmissionPolicy_AppliesTo(t *testing.T) {
	ci.Parallel(t)

	// Without a resource selection a policy applies to jobs only, exactly
	// as before resources existed.
	p := &AdmissionPolicy{Name: "p"}
	must.True(t, p.AppliesToJob("default"))
	must.False(t, p.AppliesToVariable("default", "a/b"))

	p.Match = &AdmissionPolicyMatch{Namespaces: []string{"prod"}}
	must.True(t, p.AppliesToJob("prod"))
	must.False(t, p.AppliesToJob("default"))
	must.False(t, p.AppliesToVariable("prod", "a/b"))

	p.Match.Resources = []string{AdmissionResourceVariable}
	must.False(t, p.AppliesToJob("prod"))
	must.True(t, p.AppliesToVariable("prod", "a/b"))
	must.False(t, p.AppliesToVariable("default", "a/b"))

	p.Match.VariablePaths = []string{"crd/*", "exact"}
	must.True(t, p.AppliesToVariable("prod", "crd/db-example-com/PostgresCluster/billing"))
	must.True(t, p.AppliesToVariable("prod", "exact"))
	must.False(t, p.AppliesToVariable("prod", "exact/sub"))
	must.False(t, p.AppliesToVariable("prod", "nomad/jobs/x"))

	p.Match.Resources = []string{AdmissionResourceJob, AdmissionResourceVariable}
	must.True(t, p.AppliesToJob("prod"))
	must.True(t, p.AppliesToVariable("prod", "exact"))

	c := p.Copy()
	must.Eq(t, p, c)
	c.Match.Resources[0] = "changed"
	c.Match.VariablePaths[0] = "changed"
	must.Eq(t, AdmissionResourceJob, p.Match.Resources[0])
	must.Eq(t, "crd/*", p.Match.VariablePaths[0])
}
