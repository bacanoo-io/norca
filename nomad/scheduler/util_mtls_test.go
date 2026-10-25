// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestTasksUpdated_MTLSProxy(t *testing.T) {
	ci.Parallel(t)

	j1 := mock.Job()
	j1.TaskGroups[0].Services = []*structs.Service{{
		Name: "api", PortLabel: "8080", Provider: structs.ServiceProviderNomad,
		MTLS: &structs.ServiceMTLS{Mode: structs.MTLSModeSidecar, Upstreams: []*structs.ServiceMTLSUpstream{
			{Name: "db", Service: "postgres", LocalPort: 5432},
		}},
	}}
	name := j1.TaskGroups[0].Name

	j2 := j1.Copy()
	must.False(t, tasksUpdated(j1, j2, name).modified)

	// the upstreams are written into the Envoy configuration at start
	j2.TaskGroups[0].Services[0].MTLS.Upstreams[0].Region = "eu"
	must.True(t, tasksUpdated(j1, j2, name).modified)

	// tags of the service are an in-place update
	j3 := j1.Copy()
	j3.TaskGroups[0].Services[0].Tags = []string{"v2"}
	must.False(t, tasksUpdated(j1, j3, name).modified)
}
