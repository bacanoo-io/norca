// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package wrapper

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/client/serviceregistration/checks/checkstore"
	"github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestHandlerWrapper_RemoveWorkload_ResetsRoleChecks(t *testing.T) {
	ci.Parallel(t)

	check := func(name, role string) *structs.ServiceCheck {
		return &structs.ServiceCheck{
			Name: name, Type: "http", Path: "/", Interval: time.Second, Timeout: time.Second, Role: role,
		}
	}

	workload := func(provider string) *serviceregistration.WorkloadServices {
		return &serviceregistration.WorkloadServices{
			AllocInfo: structs.AllocInfo{AllocID: "alloc1", Group: "group", Task: "task"},
			Services: []*structs.Service{{
				Name:     "web",
				Provider: provider,
				Checks: []*structs.ServiceCheck{
					check("legacy", ""),
					check("started", structs.CheckRoleStartup),
					check("alive", structs.CheckRoleLiveness),
				},
			}},
		}
	}

	for _, provider := range []string{structs.ServiceProviderNomad, structs.ServiceProviderConsul} {
		t.Run(provider, func(t *testing.T) {
			logger := testlog.HCLogger(t)
			shim := checkstore.NewStore(logger, state.NewMemDB(logger))

			w := workload(provider)
			id := func(name string) structs.CheckID {
				for _, c := range w.Services[0].Checks {
					if c.Name == name {
						return structs.NomadCheckID("alloc1", "group", c)
					}
				}
				t.Fatalf("no check %q", name)
				return ""
			}
			for _, name := range []string{"legacy", "started", "alive"} {
				must.NoError(t, shim.Set("alloc1", &structs.CheckQueryResult{ID: id(name), Status: structs.CheckSuccess}))
			}

			wrapper, _, _ := setupTestWrapper()
			wrapper.SetCheckStore(shim)
			wrapper.RemoveWorkload(w)

			results := shim.List("alloc1")

			// checks without a role are left alone
			must.Eq(t, structs.CheckSuccess, results[id("legacy")].Status)

			// liveness checks are executed by the client for both providers
			must.Eq(t, structs.CheckPending, results[id("alive")].Status)

			// startup checks only for nomad services; for consul ones the
			// client store does not hold their result anyway
			if provider == structs.ServiceProviderNomad {
				must.Eq(t, structs.CheckPending, results[id("started")].Status)
			} else {
				must.Eq(t, structs.CheckSuccess, results[id("started")].Status)
			}
		})
	}

	t.Run("no check store", func(t *testing.T) {
		wrapper, _, _ := setupTestWrapper()
		wrapper.RemoveWorkload(workload(structs.ServiceProviderNomad))
	})
}
