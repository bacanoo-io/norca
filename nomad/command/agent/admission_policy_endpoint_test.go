// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
)

func TestHTTP_AdmissionPolicy_CRUD(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		policy := &api.AdmissionPolicy{
			Name:  "p1",
			Match: &api.AdmissionPolicyMatch{Namespaces: []string{"prod"}},
			Rules: []*api.AdmissionPolicyRule{{Expression: "job.Priority < 90", Message: "too important"}},
		}

		// Create.
		req, err := http.NewRequest(http.MethodPut, "/v1/admission-policy/p1", encodeReq(policy))
		must.NoError(t, err)
		respW := httptest.NewRecorder()
		_, err = s.Server.AdmissionPolicySpecificRequest(respW, req)
		must.NoError(t, err)
		must.NotEq(t, "", respW.Header().Get("X-Nomad-Index"))

		// Name mismatch.
		req, err = http.NewRequest(http.MethodPut, "/v1/admission-policy/other", encodeReq(policy))
		must.NoError(t, err)
		_, err = s.Server.AdmissionPolicySpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "does not match")

		// Invalid CEL is rejected.
		bad := *policy
		bad.Name = "bad"
		bad.Rules = []*api.AdmissionPolicyRule{{Expression: "job.ID =="}}
		req, err = http.NewRequest(http.MethodPost, "/v1/admission-policies", encodeReq(&bad))
		must.NoError(t, err)
		_, err = s.Server.AdmissionPoliciesRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "rule 0")

		// List.
		req, err = http.NewRequest(http.MethodGet, "/v1/admission-policies", nil)
		must.NoError(t, err)
		obj, err := s.Server.AdmissionPoliciesRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.SliceLen(t, 1, obj.([]*structs.AdmissionPolicy))

		// Read.
		req, err = http.NewRequest(http.MethodGet, "/v1/admission-policy/p1", nil)
		must.NoError(t, err)
		obj, err = s.Server.AdmissionPolicySpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		got := obj.(*structs.AdmissionPolicy)
		must.Eq(t, "deny", got.Action)
		must.Eq(t, []string{"prod"}, got.Match.Namespaces)

		// Delete.
		req, err = http.NewRequest(http.MethodDelete, "/v1/admission-policy/p1", nil)
		must.NoError(t, err)
		_, err = s.Server.AdmissionPolicySpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)

		req, err = http.NewRequest(http.MethodGet, "/v1/admission-policy/p1", nil)
		must.NoError(t, err)
		_, err = s.Server.AdmissionPolicySpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "not found")
	})
}

func TestConfig_ParseAdmission(t *testing.T) {
	ci.Parallel(t)

	hcl := `
server {
  enabled = true
  admission {
    webhook "image-policy" {
      url                = "https://admission.internal:8443/validate"
      mode               = "mutate"
      timeout            = "3s"
      failure_policy     = "ignore"
      namespaces         = ["prod", "staging"]
      exclude_namespaces = ["admission"]
      ca_file            = "/etc/nomad/admission-ca.pem"
    }
    webhook "second" {
      url = "http://127.0.0.1:9000/"
    }
  }
}
`
	path := filepath.Join(t.TempDir(), "admission.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))

	cfg, err := ParseConfigFile(path)
	must.NoError(t, err)
	must.NotNil(t, cfg.Server.Admission)
	must.Len(t, 2, cfg.Server.Admission.Webhooks)

	w := cfg.Server.Admission.Webhooks[0]
	must.Eq(t, "image-policy", w.Name)
	must.Eq(t, "https://admission.internal:8443/validate", w.URL)
	must.Eq(t, config.AdmissionWebhookModeMutate, w.Mode)
	must.Eq(t, 3*time.Second, w.Timeout)
	must.Eq(t, config.AdmissionFailurePolicyIgnore, w.FailurePolicy)
	must.Eq(t, []string{"prod", "staging"}, w.Namespaces)
	must.Eq(t, []string{"admission"}, w.ExcludeNamespaces)
	must.Eq(t, "/etc/nomad/admission-ca.pem", w.CAFile)
	must.Eq(t, "second", cfg.Server.Admission.Webhooks[1].Name)

	// Unknown keys are rejected.
	path = filepath.Join(t.TempDir(), "bad.hcl")
	must.NoError(t, os.WriteFile(path, []byte(`server { admission { webhook "x" { nope = 1 } } }`), 0o600))
	_, err = ParseConfigFile(path)
	must.ErrorContains(t, err, "nope")

	// Merge appends and overrides by name.
	merged := DefaultConfig().Merge(cfg)
	must.Len(t, 2, merged.Server.Admission.Webhooks)
}
