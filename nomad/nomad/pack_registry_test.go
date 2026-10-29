// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func TestPackRegistry_LifecycleAndServerRender(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	testutil.WaitForLeader(t, s.RPC)

	req := &structs.GenericRequest{
		QueryOptions: structs.QueryOptions{
			Region:    "global",
			Namespace: structs.DefaultNamespace,
		},
	}

	// 1. Upsert the built-in official catalog registry.
	reg, idx, err := s.UpsertPackRegistry(req, &PackRegistry{
		Name: "official",
		Spec: &PackRegistrySpec{
			Type:       "dir",
			Source:     BuiltinRegistrySource,
			DefaultRef: "latest",
		},
	})
	must.NoError(t, err)
	must.Positive(t, idx)
	must.Eq(t, "official", reg.Name)
	must.Eq(t, PackRegistryPhaseReady, reg.Status.Phase)
	must.Eq(t, 4, reg.Status.PackCount)

	// 2. List registries.
	regs, _, err := s.ListPackRegistries(req)
	must.NoError(t, err)
	must.Len(t, 1, regs)
	must.Eq(t, "official", regs[0].Name)

	// 3. List catalog packs across registries.
	packs, _, err := s.ListCatalogPacks(req, "")
	must.NoError(t, err)
	must.Len(t, 4, packs)
	for _, p := range packs {
		must.True(t, p.Compatible, must.Sprintf("pack %s is incompatible: %v", p.Name, p.MissingFeatures))
		must.True(t, p.HasSchema)
	}

	// 4. Fetch pack detail and synthesized UIFormSchema for redis.
	detail, _, err := s.GetCatalogPack(req, "official", "redis", "")
	must.NoError(t, err)
	must.NotNil(t, detail)
	must.Eq(t, "redis", detail.Pack.Name)
	must.StrContains(t, detail.Readme, "Redis Pack for Norca")
	must.NotNil(t, detail.UIFormSchema)
	tabs, ok := detail.UIFormSchema["x-ui-tabs"].([]any)
	must.True(t, ok)
	must.Positive(t, len(tabs))
	props, ok := detail.UIFormSchema["properties"].(map[string]any)
	must.True(t, ok)
	cpuProp, ok := props["cpu"].(map[string]any)
	must.True(t, ok)
	must.Eq[any](t, "MHz", cpuProp["x-ui-unit"])
	must.Eq[any](t, 500, cpuProp["default"])

	// 5. Render the redis pack on the server with custom values and secret.
	deployReq := &PackDeployRequest{
		Registry:    "official",
		Pack:        "redis",
		Description: "Deployed from UI catalog",
		Values: map[string]any{
			"job_name":     "my-redis",
			"count":        2,
			"cpu":          1000,
			"memory":       512,
			"auth_enabled": true,
		},
		Secrets: []*structs.ReleaseVariableSecret{
			{
				Path:  "nomad/jobs/redis",
				Items: map[string]string{"password": "super-secret-value"},
			},
		},
	}
	renderedReq, err := s.RenderCatalogPack(req, "my-redis", deployReq)
	must.NoError(t, err)
	must.NotNil(t, renderedReq)
	must.Eq(t, "redis", renderedReq.Pack.Name)
	must.Eq(t, "official", renderedReq.Pack.Registry)
	must.Eq(t, BuiltinRegistrySource, renderedReq.Pack.Source)
	must.Eq(t, "latest", renderedReq.Pack.Ref)
	must.Len(t, 2, renderedReq.Resources)
	must.Eq[any](t, 2, renderedReq.Values["count"])
	must.Eq[any](t, 1000, renderedReq.Values["cpu"])

	// 6. Sync and delete the registry.
	synced, _, err := s.SyncPackRegistry(req, "official")
	must.NoError(t, err)
	must.Eq(t, PackRegistryPhaseReady, synced.Status.Phase)

	_, err = s.DeletePackRegistry(req, "official")
	must.NoError(t, err)
	regsAfter, _, err := s.ListPackRegistries(req)
	must.NoError(t, err)
	must.Len(t, 0, regsAfter)
}
