// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rules

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/pack/sdk/pack/variables"
	"github.com/shoenig/test/must"
	"github.com/zclconf/go-cty/cty"
)

const testRules = `
variable "tls_required" {
  type    = bool
  default = false
}

rule "tls_mode" {
  condition     = !var.tls_required || contains(["mtls", "secret"], var.tls_mode)
  error_message = "tls_mode must be mtls or secret when tls_required is set, not ${var.tls_mode}."
}

rule "env_names" {
  condition = alltrue([
    for k in keys(var.extra_env) : can(regex("^APP_[A-Z0-9_]+$", k)) && !can(regex("(SECRET|TOKEN)", k))
  ])
  error_message = "extra_env names must be APP_* and not hold secrets."
}

rule "databases" {
  condition     = alltrue([for d in var.databases : d.name != "" && !contains(["postgres", "template0"], d.name)]) && length(distinct([for d in var.databases : d.name])) == length(var.databases)
  error_message = "database names must be set, unique and not reserved."
}
`

func vars(values map[string]cty.Value) map[variables.ID]*variables.Variable {
	out := map[variables.ID]*variables.Variable{}
	for k, v := range values {
		out[variables.ID(k)] = &variables.Variable{Name: variables.ID(k), Value: v}
	}
	return out
}

func db(name string) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal(name)})
}

func TestRules(t *testing.T) {
	ci.Parallel(t)
	rules, diags := Parse("variables.hcl", []byte(testRules))
	must.False(t, diags.HasErrors(), must.Sprint(diags))
	must.Len(t, 3, rules)

	good := map[string]cty.Value{
		"tls_required": cty.True,
		"tls_mode":     cty.StringVal("mtls"),
		"extra_env":    cty.MapVal(map[string]cty.Value{"APP_LOG": cty.StringVal("debug")}),
		"databases":    cty.ListVal([]cty.Value{db("app"), db("audit")}),
	}
	must.NoError(t, Evaluate(rules, vars(good)))

	bad := map[string]cty.Value{
		"tls_required": cty.True,
		"tls_mode":     cty.StringVal("none"),
		"extra_env":    cty.MapVal(map[string]cty.Value{"APP_TOKEN": cty.StringVal("x")}),
		"databases":    cty.ListVal([]cty.Value{db("app"), db("app")}),
	}
	err := Evaluate(rules, vars(bad))
	must.ErrorContains(t, err, `tls_mode must be mtls or secret when tls_required is set, not none. (rule "tls_mode")`)
	must.ErrorContains(t, err, `rule "env_names"`)
	must.ErrorContains(t, err, `rule "databases"`)

	// An unknown variable is reported, not a pass.
	err = Evaluate(rules, vars(map[string]cty.Value{"tls_required": cty.True}))
	must.ErrorContains(t, err, `rule "tls_mode"`)
}

func TestRules_ParseErrors(t *testing.T) {
	ci.Parallel(t)
	cases := map[string]string{
		"missing condition": `rule "a" { error_message = "x" }`,
		"missing message":   `rule "a" { condition = true }`,
		"duplicate":         "rule \"a\" {\n condition = true\n error_message = \"x\"\n}\nrule \"a\" {\n condition = true\n error_message = \"x\"\n}",
		"other reference":   `rule "a" {` + "\n condition = local.x\n error_message = \"x\"\n}",
		"bad name":          "rule \"1a\" {\n condition = true\n error_message = \"x\"\n}",
	}
	for name, src := range cases {
		_, diags := Parse("variables.hcl", []byte(src))
		must.True(t, diags.HasErrors(), must.Sprint(name))
	}

	// Variable blocks are left to the variable parser.
	rules, diags := Parse("variables.hcl", []byte(`variable "x" { default = 1 }`))
	must.False(t, diags.HasErrors())
	must.SliceEmpty(t, rules)
}
