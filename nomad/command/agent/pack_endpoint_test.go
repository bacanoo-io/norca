// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestHTTP_Packs_InstallFromCatalog(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		call := func(method, path string, body any) (any, error) {
			var buf bytes.Buffer
			if body != nil {
				must.NoError(t, json.NewEncoder(&buf).Encode(body))
			}
			req, err := http.NewRequest(method, path, &buf)
			must.NoError(t, err)
			resp := httptest.NewRecorder()
			switch {
			case path == "/v1/pack-registry":
				return s.Server.PackRegistryUpsertRequest(resp, req)
			case strings.HasPrefix(path, "/v1/pack/"):
				return s.Server.PackDetailRequest(resp, req)
			case strings.HasPrefix(path, "/v1/packs"):
				return s.Server.PacksCatalogRequest(resp, req)
			default:
				return s.Server.ReleaseSpecificRequest(resp, req)
			}
		}

		out, err := call(http.MethodPost, "/v1/pack-registry", map[string]any{
			"Name": "builtin",
			"Spec": map[string]any{"type": "dir", "source": "builtin", "syncInterval": "1h"},
		})
		must.NoError(t, err)
		reg := out.(*nomad.PackRegistry)
		must.Eq(t, nomad.PackRegistryPhaseReady, reg.Status.Phase)
		must.Eq(t, "1h", reg.Spec.SyncInterval)

		out, err = call(http.MethodGet, "/v1/packs", nil)
		must.NoError(t, err)
		must.Len(t, 4, out.([]*nomad.PackCatalogEntry))

		out, err = call(http.MethodGet, "/v1/pack/builtin/redis", nil)
		must.NoError(t, err)
		detail := out.(*nomad.PackDetailResponse)
		props := detail.UIFormSchema["properties"].(map[string]any)
		pass := props["redis_password"].(map[string]any)
		must.Eq[any](t, "nomad/jobs/redis", pass["x-ui-secret-path"], must.Sprint("from the secret block"))

		deploy := map[string]any{
			"Registry": "builtin",
			"Pack":     "redis",
			"Values": map[string]any{
				"auth_enabled":   true,
				"redis_password": "pa55word-from-values",
				"count":          2,
			},
		}
		out, err = call(http.MethodPost, "/v1/release/cache/plan-pack", deploy)
		must.NoError(t, err)
		plan := out.(structs.ReleaseApplyResponse)
		types := map[string]bool{}
		for _, c := range plan.Changes {
			types[c.Type] = true
		}
		must.True(t, types[structs.ReleaseResourceJob])
		must.True(t, types[structs.ReleaseResourceVariable])

		out, err = call(http.MethodPost, "/v1/release/cache/deploy-pack", deploy)
		must.NoError(t, err)
		applied := out.(structs.ReleaseApplyResponse)
		must.NotNil(t, applied.Release)
		must.Eq(t, "builtin", applied.Release.Spec.Pack.Registry)
		rev, _, err := s.Client().Releases().Revision("cache", applied.Revision, nil)
		must.NoError(t, err)
		must.Eq[any](t, float64(2), rev.Spec.Values["count"])
		_, stored := rev.Spec.Values["redis_password"]
		must.False(t, stored, must.Sprint("the secret variable is not a release value"))

		b, err := json.Marshal(applied)
		must.NoError(t, err)
		must.StrNotContains(t, string(b), "pa55word-from-values")
	})
}
