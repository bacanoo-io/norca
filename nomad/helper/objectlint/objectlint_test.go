// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package objectlint

import (
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/shoenig/test/must"
)

const lintSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["port"],
      "properties": {
        "port":  {"type": "integer", "minimum": 1},
        "class": {"type": "string", "default": "nomad"}
      }
    },
    "status": {
      "type": "object",
      "additionalProperties": false,
      "properties": {"observedGeneration": {"type": "integer"}}
    }
  }
}`

func findings(r *Report, sev Severity) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Severity == sev {
			out = append(out, f.String())
		}
	}
	return out
}

func TestLint(t *testing.T) {
	kind := &api.ObjectKind{
		Group: "norca.io", Kind: "LoadBalancer", Namespaced: true, Schema: lintSchema,
		Rules:    []*api.ObjectKindRule{{Expression: "self.spec.port != 22", Message: "port 22 is reserved"}},
		Webhooks: []*api.ObjectKindWebhook{{Name: "v", URL: "https://127.0.0.1:8443/v", FailurePolicy: "ignore"}},
	}
	samples := []*api.CustomObject{
		{Name: "ok", Namespace: "default", Spec: map[string]any{"port": 80}},
		{Name: "ssh", Namespace: "default", Spec: map[string]any{"port": 22}},
		{Name: "bad", Namespace: "default", Spec: map[string]any{"port": "x"}},
		{Group: "other.io", Kind: "X", Name: "foreign"},
	}
	r, err := Lint(kind, samples)
	must.NoError(t, err)
	must.True(t, r.HasErrors())

	errs := strings.Join(findings(r, SeverityError), "\n")
	must.StrContains(t, errs, `status does not allow "conditions"`)
	must.StrContains(t, errs, "object default/ssh: port 22 is reserved")
	must.StrContains(t, errs, "object default/bad")
	must.StrContains(t, errs, "is of kind other.io/X")
	must.StrNotContains(t, errs, "default/ok")

	warns := strings.Join(findings(r, SeverityWarning), "\n")
	must.StrContains(t, warns, "no description")
	must.StrContains(t, warns, "loopback")
	must.StrContains(t, warns, "no ca_cert")
	must.StrContains(t, warns, `failure_policy "ignore"`)

	// Defaults are filled in.
	must.Eq(t, "nomad", r.Objects[0].Spec["class"])

	// A broken kind: schema, rule and migration don't compile.
	kind = &api.ObjectKind{
		Group: "norca.io", Kind: "LoadBalancer", Description: "x", Schema: `{"type": 5}`,
		Rules:     []*api.ObjectKindRule{{Expression: "self.spec."}},
		Migration: &api.ObjectKindMigration{Spec: "1"},
	}
	r, err = Lint(kind, nil)
	must.NoError(t, err)
	errs = strings.Join(findings(r, SeverityError), "\n")
	must.StrContains(t, errs, "schema:")
	must.StrContains(t, errs, "rules:")
	must.StrContains(t, errs, "migration:")
	warns = strings.Join(findings(r, SeverityWarning), "\n")
	must.StrContains(t, warns, "raise the version")
}
