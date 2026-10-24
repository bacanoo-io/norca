// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestAdmissionConfig_ValidateResources(t *testing.T) {
	ci.Parallel(t)

	validate := func(w *AdmissionWebhookConfig) error {
		w.Name = "hook"
		w.URL = "https://x:8443/validate"
		c := &AdmissionConfig{Webhooks: []*AdmissionWebhookConfig{w}}
		c.Canonicalize()
		return c.Validate()
	}

	must.NoError(t, validate(&AdmissionWebhookConfig{
		Resources:            []string{"job", "variable"},
		VariablePaths:        []string{"crd/*"},
		IncludeVariableItems: true,
	}))
	must.ErrorContains(t, validate(&AdmissionWebhookConfig{Resources: []string{"node"}}), "invalid resource")
	must.ErrorContains(t, validate(&AdmissionWebhookConfig{
		Mode:      AdmissionWebhookModeMutate,
		Resources: []string{"variable"},
	}), "mutating webhooks cannot apply")
	must.ErrorContains(t, validate(&AdmissionWebhookConfig{IncludeVariableItems: true}), "require the")
	must.ErrorContains(t, validate(&AdmissionWebhookConfig{VariablePaths: []string{"a"}}), "require the")

	w := &AdmissionWebhookConfig{Resources: []string{"variable"}, VariablePaths: []string{"a"}}
	c := w.Copy()
	c.Resources[0] = "job"
	c.VariablePaths[0] = "b"
	must.Eq(t, "variable", w.Resources[0])
	must.Eq(t, "a", w.VariablePaths[0])
}
