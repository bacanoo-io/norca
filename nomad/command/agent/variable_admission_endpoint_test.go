// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestConfig_ParseAdmissionVariables(t *testing.T) {
	ci.Parallel(t)

	hcl := `
server {
  enabled = true
  admission {
    webhook "crd" {
      url                    = "https://admission.internal:8443/validate"
      resources              = ["job", "variable"]
      variable_paths         = ["crd/*"]
      include_variable_items = true
    }
  }
}
`
	path := filepath.Join(t.TempDir(), "admission.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))

	cfg, err := ParseConfigFile(path)
	must.NoError(t, err)
	must.Len(t, 1, cfg.Server.Admission.Webhooks)
	w := cfg.Server.Admission.Webhooks[0]
	must.Eq(t, []string{"job", "variable"}, w.Resources)
	must.Eq(t, []string{"crd/*"}, w.VariablePaths)
	must.True(t, w.IncludeVariableItems)
}

func TestHTTP_Variables_Admission(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		client := s.Client()

		_, err := client.AdmissionPolicies().Register(&api.AdmissionPolicy{
			Name:   "owner",
			Action: api.AdmissionPolicyActionWarn,
			Match: &api.AdmissionPolicyMatch{
				Resources:     []string{api.AdmissionResourceVariable},
				VariablePaths: []string{"crd/*"},
			},
			Rules: []*api.AdmissionPolicyRule{
				{Expression: "request.operation == 'delete' || has(variable.items.owner)", Message: "missing owner"},
				{Expression: "request.operation != 'delete' || oldVariable.items.owner != 'nobody'", Message: "deleting an orphan"},
			},
		}, nil)
		must.NoError(t, err)
		_, err = client.AdmissionPolicies().Register(&api.AdmissionPolicy{
			Name: "no-huge",
			Match: &api.AdmissionPolicyMatch{
				Resources:     []string{api.AdmissionResourceVariable},
				VariablePaths: []string{"crd/*"},
			},
			Rules: []*api.AdmissionPolicyRule{
				{Expression: "!has(variable.items.size) || variable.items.size != 'huge'", Message: "too big"},
			},
		}, nil)
		must.NoError(t, err)

		// The warnings come back in the write metadata.
		v := &api.Variable{Path: "crd/g/k/n", Items: api.VariableItems{"spec": "x"}}
		_, wm, err := client.Variables().Create(v, nil)
		must.NoError(t, err)
		must.Len(t, 1, wm.Warnings)
		must.StrContains(t, wm.Warnings[0], "missing owner")

		v = &api.Variable{Path: "crd/g/k/n", Items: api.VariableItems{"owner": "nobody"}}
		out, wm, err := client.Variables().Update(v, nil)
		must.NoError(t, err)
		must.Len(t, 0, wm.Warnings)

		// Checked writes carry the warnings too.
		v = &api.Variable{Path: "crd/g/k/n", Items: api.VariableItems{"a": "b"}, ModifyIndex: out.ModifyIndex}
		_, wm, err = client.Variables().CheckedUpdate(v, nil)
		must.NoError(t, err)
		must.Len(t, 1, wm.Warnings)

		// A denial is a 400 with the policy message.
		v = &api.Variable{Path: "crd/g/k/n", Items: api.VariableItems{"owner": "a", "size": "huge"}}
		_, _, err = client.Variables().Create(v, nil)
		must.ErrorContains(t, err, "400")
		must.ErrorContains(t, err, "variable write denied by admission control")
		must.ErrorContains(t, err, "too big")

		// Deletes return the warnings as well.
		_, _, err = client.Variables().Update(&api.Variable{Path: "crd/g/k/n", Items: api.VariableItems{"owner": "nobody"}}, nil)
		must.NoError(t, err)
		wm, err = client.Variables().Delete("crd/g/k/n", nil)
		must.NoError(t, err)
		must.Len(t, 1, wm.Warnings)
		must.StrContains(t, wm.Warnings[0], "deleting an orphan")

		// Other paths are not affected.
		_, wm, err = client.Variables().Create(&api.Variable{Path: "other", Items: api.VariableItems{"size": "huge"}}, nil)
		must.NoError(t, err)
		must.Len(t, 0, wm.Warnings)
	})
}
