// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestHTTP_Quota_CRUD(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		// Register through the api representation, as the CLI does
		spec := &api.QuotaSpec{
			Name: "team-a",
			Limits: []*api.QuotaLimit{{
				Region:      "global",
				RegionLimit: &api.Resources{CPU: pointer.Of(1000), MemoryMB: pointer.Of(2048)},
				Allocations: pointer.Of(10),
			}},
			TaskDefaults: &api.Resources{CPU: pointer.Of(200), MemoryMB: pointer.Of(256)},
			Enforce:      "submit",
		}
		body, err := json.Marshal(spec)
		must.NoError(t, err)

		req, err := http.NewRequest(http.MethodPut, "/v1/quota", bytes.NewReader(body))
		must.NoError(t, err)
		respW := httptest.NewRecorder()
		_, err = s.Server.QuotaCreateRequest(respW, req)
		must.NoError(t, err)
		must.NotEq(t, "", respW.Result().Header.Get("X-Nomad-Index"))

		// Name mismatch with the path
		req, err = http.NewRequest(http.MethodPost, "/v1/quota/other", bytes.NewReader(body))
		must.NoError(t, err)
		_, err = s.Server.QuotaSpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "does not match")

		// List
		req, err = http.NewRequest(http.MethodGet, "/v1/quotas", nil)
		must.NoError(t, err)
		obj, err := s.Server.QuotasRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.Len(t, 1, obj.([]*structs.QuotaSpec))

		// Get
		req, err = http.NewRequest(http.MethodGet, "/v1/quota/team-a", nil)
		must.NoError(t, err)
		obj, err = s.Server.QuotaSpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		out := obj.(*structs.QuotaSpec)
		must.Eq(t, structs.QuotaEnforceSubmit, out.Enforce)
		must.Eq(t, 10, *out.Limits[0].Allocations)
		must.Eq(t, 200, out.TaskDefaults.CPU)

		// Usage
		req, err = http.NewRequest(http.MethodGet, "/v1/quota/usage/team-a", nil)
		must.NoError(t, err)
		obj, err = s.Server.QuotaSpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		usage := obj.(*structs.QuotaUsage)
		must.NotNil(t, usage.Used[out.Limits[0].HashKey()])

		req, err = http.NewRequest(http.MethodGet, "/v1/quota-usages", nil)
		must.NoError(t, err)
		obj, err = s.Server.QuotaUsagesRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.Len(t, 1, obj.([]*structs.QuotaUsage))

		// Delete
		req, err = http.NewRequest(http.MethodDelete, "/v1/quota/team-a", nil)
		must.NoError(t, err)
		_, err = s.Server.QuotaSpecificRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)

		req, err = http.NewRequest(http.MethodGet, "/v1/quota/team-a", nil)
		must.NoError(t, err)
		_, err = s.Server.QuotaSpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "not found")

		// Usage only supports GET
		req, err = http.NewRequest(http.MethodDelete, "/v1/quota/usage/team-a", nil)
		must.NoError(t, err)
		_, err = s.Server.QuotaSpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, ErrInvalidMethod)
	})
}
