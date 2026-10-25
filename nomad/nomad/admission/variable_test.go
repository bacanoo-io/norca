// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func variablePolicy(name, action string, exprs ...string) *structs.AdmissionPolicy {
	p := testPolicy(name, action, exprs...)
	p.Match = &structs.AdmissionPolicyMatch{Resources: []string{structs.AdmissionResourceVariable}}
	return p
}

func TestPolicyEngine_EvaluateVariable(t *testing.T) {
	ci.Parallel(t)

	e, err := NewPolicyEngine()
	must.NoError(t, err)
	ctx := context.Background()

	newVar := &VariableInput{
		Namespace: "default",
		Path:      "crd/db-example-com/PostgresCluster/billing",
		Items:     map[string]string{"spec": `{"replicas":3}`, "owner": "team-a"},
	}
	oldVar := &VariableInput{
		Namespace:   "default",
		Path:        newVar.Path,
		Items:       map[string]string{"owner": "team-b"},
		CreateIndex: 10,
		ModifyIndex: 12,
	}
	req := map[string]any{"resource": "variable", "operation": "set", "namespace": "default", "path": newVar.Path}
	eval := func(p *structs.AdmissionPolicy, old *VariableInput) []*Violation {
		return e.EvaluateVariable(ctx, []*structs.AdmissionPolicy{p}, newVar.Value("set"), old.Value(""), req)
	}

	// The CEL rules must compile with the variable inputs.
	must.NoError(t, e.Compile(variablePolicy("c", "deny",
		`variable.path.startsWith('crd/')`,
		`oldVariable == null || oldVariable.items.owner == variable.items.owner`,
		`request.resource == 'variable' && request.operation in ['set', 'delete']`,
	)))

	// allow
	must.Len(t, 0, eval(variablePolicy("allow", "deny",
		`variable.namespace == 'default'`,
		`variable.operation == 'set'`,
		`has(variable.items.spec) && variable.items.owner == 'team-a'`,
	), nil))

	// deny and warn keep the action of the policy
	v := eval(variablePolicy("deny", "deny", `!variable.path.startsWith('crd/')`), nil)
	must.Len(t, 1, v)
	must.Eq(t, "deny", v[0].Action)
	must.StrContains(t, v[0].Message, `admission policy "deny"`)
	v = eval(variablePolicy("warn", "warn", `has(variable.items.description)`), nil)
	must.Len(t, 1, v)
	must.Eq(t, "warn", v[0].Action)

	// oldVariable is null for a create and set for an update
	immutableOwner := variablePolicy("owner", "deny",
		`oldVariable == null || oldVariable.items.owner == variable.items.owner`)
	must.Len(t, 0, eval(immutableOwner, nil))
	v = eval(immutableOwner, oldVar)
	must.Len(t, 1, v)
	must.Len(t, 0, eval(variablePolicy("idx", "deny",
		`oldVariable.modify_index == 12 && oldVariable.create_index == 10`), oldVar))

	// delete: no items, the old Variable is available
	del := e.EvaluateVariable(ctx, []*structs.AdmissionPolicy{variablePolicy("del", "deny",
		`variable.operation != 'delete' || oldVariable.items.owner != 'team-b'`)},
		(&VariableInput{Namespace: "default", Path: newVar.Path}).Value("delete"), oldVar.Value(""),
		map[string]any{"operation": "delete"})
	must.Len(t, 1, del)
	must.Len(t, 0, e.EvaluateVariable(ctx, []*structs.AdmissionPolicy{variablePolicy("del-items", "deny",
		`size(variable.items) == 0`)},
		(&VariableInput{Namespace: "default", Path: newVar.Path}).Value("delete"), nil,
		map[string]any{"operation": "delete"}))

	// a rule that reads the job of a Variables write fails closed
	v = eval(variablePolicy("job", "deny", `job.ID == 'x'`), nil)
	must.Len(t, 1, v)
	must.StrContains(t, v[0].Message, "failed to evaluate")
}

func TestPolicyEngine_EvaluateJobVariableNull(t *testing.T) {
	ci.Parallel(t)

	e, err := NewPolicyEngine()
	must.NoError(t, err)

	raw, err := EncodeJob(mock.Job())
	must.NoError(t, err)
	jobVal, err := JobValue(raw)
	must.NoError(t, err)

	// Job evaluation is unchanged; the variable inputs are null.
	v := e.Evaluate(context.Background(), []*structs.AdmissionPolicy{
		testPolicy("p", "deny", `variable == null && oldVariable == null`, `job.Type == 'service'`),
	}, jobVal, map[string]any{"operation": "register"})
	must.Len(t, 0, v)
}

func TestCRDSchemaPath(t *testing.T) {
	ci.Parallel(t)

	p, ok := CRDSchemaPath("crd/db-example-com/PostgresCluster/billing")
	must.True(t, ok)
	must.Eq(t, "crd-schema/db-example-com/PostgresCluster", p)

	p, ok = CRDSchemaPath("crd/g/k/name/with/slashes")
	must.True(t, ok)
	must.Eq(t, "crd-schema/g/k", p)

	for _, path := range []string{"crd/g/k", "crd/g//n", "crd-schema/g/k", "nomad/jobs/x", "crd//k/n"} {
		_, ok = CRDSchemaPath(path)
		must.False(t, ok, must.Sprint(path))
	}

	must.True(t, IsCRDSchemaPath("crd-schema/g/k"))
	must.False(t, IsCRDSchemaPath("crd-schema/g"))
	must.False(t, IsCRDSchemaPath("crd-schema/g/k/x"))
	must.False(t, IsCRDSchemaPath("crd/g/k/n"))
}

const testSchema = `{
  "type": "object",
  "required": ["spec"],
  "properties": {
    "spec": {
      "type": "object",
      "required": ["replicas"],
      "properties": {"replicas": {"type": "integer", "minimum": 1, "maximum": 5}},
      "additionalProperties": false
    },
    "owner": {"type": "string", "pattern": "^team-"}
  }
}`

func TestVariableSchema(t *testing.T) {
	ci.Parallel(t)

	// JSON encoding: item values are parsed as JSON.
	s, err := CompileVariableSchema(map[string]string{"schema": testSchema, "encoding": "json"})
	must.NoError(t, err)
	must.NoError(t, s.Validate(map[string]string{"spec": `{"replicas":3}`, "owner": `"team-a"`}))

	err = s.Validate(map[string]string{"spec": `{"replicas":7}`})
	t.Logf("validation error: %v", err)
	must.ErrorContains(t, err, `at "/spec/replicas"`)
	must.ErrorContains(t, s.Validate(map[string]string{"owner": `"team-a"`}), "spec")
	must.ErrorContains(t, s.Validate(map[string]string{"spec": `{"replicas":`}), `item "spec" is not valid JSON`)

	// String encoding (default): items are validated as strings.
	s, err = CompileVariableSchema(map[string]string{"schema": `{
		"type": "object",
		"required": ["size"],
		"properties": {"size": {"enum": ["small", "large"]}}
	}`})
	must.NoError(t, err)
	must.NoError(t, s.Validate(map[string]string{"size": "small"}))
	must.Error(t, s.Validate(map[string]string{"size": "medium"}))

	// Invalid schemas.
	_, err = CompileVariableSchema(map[string]string{})
	must.ErrorContains(t, err, `missing "schema" item`)
	_, err = CompileVariableSchema(map[string]string{"schema": `{`})
	must.ErrorContains(t, err, "invalid JSON")
	_, err = CompileVariableSchema(map[string]string{"schema": `{"type": 7}`})
	must.ErrorContains(t, err, "invalid JSON Schema")
	_, err = CompileVariableSchema(map[string]string{"schema": `{}`, "encoding": "yaml"})
	must.ErrorContains(t, err, "invalid")

	// References to anything but the schema itself are never loaded.
	_, err = CompileVariableSchema(map[string]string{"schema": `{"$ref": "file:///etc/passwd"}`})
	must.ErrorContains(t, err, "not allowed")
	_, err = CompileVariableSchema(map[string]string{"schema": `{"$ref": "https://example.com/schema.json"}`})
	must.ErrorContains(t, err, "not allowed")
	s, err = CompileVariableSchema(map[string]string{"schema": `{
		"$defs": {"name": {"type": "string", "maxLength": 3}},
		"properties": {"a": {"$ref": "#/$defs/name"}}
	}`})
	must.NoError(t, err)
	must.Error(t, s.Validate(map[string]string{"a": "abcd"}))
}

func TestSchemaCache(t *testing.T) {
	ci.Parallel(t)

	c := NewSchemaCache()
	items := map[string]string{"schema": `{"required": ["a"]}`}
	s1, err := c.Schema("default", "crd-schema/g/k", 5, items)
	must.NoError(t, err)
	s2, err := c.Schema("default", "crd-schema/g/k", 5, map[string]string{"schema": "ignored, cached"})
	must.NoError(t, err)
	must.True(t, s1 == s2)

	// A new modify index recompiles.
	_, err = c.Schema("default", "crd-schema/g/k", 6, map[string]string{"schema": "{"})
	must.Error(t, err)
}

func TestVariableInput_Review(t *testing.T) {
	ci.Parallel(t)

	v := &VariableInput{Namespace: "default", Path: "a/b", Items: map[string]string{"secret": "s3cr3t"}, ModifyIndex: 3}
	b, err := json.Marshal(v.Review(false))
	must.NoError(t, err)
	must.StrNotContains(t, string(b), "s3cr3t")
	must.StrNotContains(t, string(b), "secret")

	b, err = json.Marshal(v.Review(true))
	must.NoError(t, err)
	must.StrContains(t, string(b), `"secret":"s3cr3t"`)

	var nilVar *VariableInput
	must.Nil(t, nilVar.Review(true))
	must.Nil(t, nilVar.Value(""))
}

func TestWebhook_AppliesToVariable(t *testing.T) {
	ci.Parallel(t)

	// Webhooks without resources only see jobs.
	cfg := webhookConfig("hook", "http://127.0.0.1:1")
	w, err := NewWebhook(cfg)
	must.NoError(t, err)
	must.True(t, w.AppliesTo("default"))
	must.False(t, w.AppliesToVariable("default", "a"))

	cfg.Resources = []string{"variable"}
	cfg.VariablePaths = []string{"crd/*"}
	cfg.ExcludeNamespaces = []string{"system"}
	must.False(t, w.AppliesTo("default"))
	must.True(t, w.AppliesToVariable("default", "crd/g/k/n"))
	must.False(t, w.AppliesToVariable("default", "other"))
	must.False(t, w.AppliesToVariable("system", "crd/g/k/n"))
	must.False(t, w.IncludeVariableItems())

	// Mutating webhooks never see Variables.
	cfg.Mode = "mutate"
	must.False(t, w.AppliesToVariable("default", "crd/g/k/n"))
}
