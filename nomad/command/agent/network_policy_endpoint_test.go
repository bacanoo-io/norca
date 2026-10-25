// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestHTTP_NetworkPolicy_CRUD(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		policy := &api.NetworkPolicy{
			Name:     "db",
			Selector: map[string]string{"app": "db"},
			Egress: []*api.NetworkPolicyRule{{
				Peers: []*api.NetworkPolicyPeer{{CIDR: []string{"10.0.0.0/8"}}},
				Ports: []string{"443/tcp"},
			}},
		}

		req, err := http.NewRequest(http.MethodPut, "/v1/network-policy/db", encodeReq(policy))
		must.NoError(t, err)
		respW := httptest.NewRecorder()
		_, err = s.Server.NetworkPolicySpecificRequest(respW, req)
		must.NoError(t, err)
		must.NotEq(t, "", respW.Header().Get("X-Nomad-Index"))

		req, err = http.NewRequest(http.MethodPut, "/v1/network-policy/other", encodeReq(policy))
		must.NoError(t, err)
		_, err = s.Server.NetworkPolicySpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "does not match")

		req, err = http.NewRequest(http.MethodGet, "/v1/network-policies", nil)
		must.NoError(t, err)
		obj, err := s.Server.NetworkPoliciesRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.SliceLen(t, 1, obj.([]*structs.NetworkPolicy))

		req, err = http.NewRequest(http.MethodGet, "/v1/network-policy/db", nil)
		must.NoError(t, err)
		obj, err = s.Server.NetworkPolicySpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.Eq(t, "default", obj.(*structs.NetworkPolicy).Namespace)

		req, err = http.NewRequest(http.MethodGet, "/v1/network-policy/db/status", nil)
		must.NoError(t, err)
		obj, err = s.Server.NetworkPolicySpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.SliceLen(t, 0, obj.([]*structs.NetworkPolicyAllocStatus))

		req, err = http.NewRequest(http.MethodDelete, "/v1/network-policy/db", nil)
		must.NoError(t, err)
		_, err = s.Server.NetworkPolicySpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)

		req, err = http.NewRequest(http.MethodGet, "/v1/network-policy/db", nil)
		must.NoError(t, err)
		_, err = s.Server.NetworkPolicySpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "not found")

		// Admin policies.
		admin := &api.AdminNetworkPolicy{
			Name:     "adm",
			Priority: 5,
			Egress:   []*api.NetworkPolicyRule{{Action: api.NetworkPolicyActionDeny}},
		}
		req, err = http.NewRequest(http.MethodPost, "/v1/admin-network-policies", encodeReq(admin))
		must.NoError(t, err)
		_, err = s.Server.AdminNetworkPoliciesRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)

		req, err = http.NewRequest(http.MethodGet, "/v1/admin-network-policy/adm", nil)
		must.NoError(t, err)
		obj, err = s.Server.AdminNetworkPolicySpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.Eq(t, 5, obj.(*structs.AdminNetworkPolicy).Priority)

		req, err = http.NewRequest(http.MethodGet, "/v1/admin-network-policy/adm/status", nil)
		must.NoError(t, err)
		_, err = s.Server.AdminNetworkPolicySpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)

		req, err = http.NewRequest(http.MethodDelete, "/v1/admin-network-policy/adm", nil)
		must.NoError(t, err)
		_, err = s.Server.AdminNetworkPolicySpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
	})
}
