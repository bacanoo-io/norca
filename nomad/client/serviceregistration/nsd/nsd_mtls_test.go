// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nsd

import (
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestServiceRegistrationHandler_MTLSRegistrations(t *testing.T) {
	ci.Parallel(t)

	h := NewServiceRegistrationHandler(hclog.NewNullLogger(), &ServiceRegistrationHandlerCfg{
		Enabled: true, CheckWatcher: new(mockCheckWatcher),
	}).(*ServiceRegistrationHandler)

	workload := mockWorkload()
	workload.Ports = append(workload.Ports, structs.AllocatedPortMapping{
		Label: structs.MTLSProxyPortLabel("redis-db"), HostIP: "10.10.13.2", Value: 25000, To: -1,
	})

	// a sidecar mode service is registered with the port of its sidecar
	sidecar := workload.Services[0].Copy()
	sidecar.MTLS = &structs.ServiceMTLS{Mode: structs.MTLSModeSidecar}
	sidecar.Meta = map[string]string{"team": "a"}
	reg, err := h.generateNomadServiceRegistration(sidecar, workload)
	must.NoError(t, err)
	must.Eq(t, 25000, reg.Port)
	must.Eq(t, "10.10.13.2", reg.Address)
	must.Eq(t, map[string]string{"team": "a", structs.MTLSServiceMetaKey: structs.MTLSModeSidecar}, reg.Meta)
	// the service meta is not modified
	must.Eq(t, map[string]string{"team": "a"}, sidecar.Meta)

	// an app mode service keeps its port and gets the mode meta
	app := workload.Services[1].Copy()
	app.MTLS = &structs.ServiceMTLS{}
	reg, err = h.generateNomadServiceRegistration(app, workload)
	must.NoError(t, err)
	must.Eq(t, 24098, reg.Port)
	must.Eq(t, map[string]string{structs.MTLSServiceMetaKey: structs.MTLSModeApp}, reg.Meta)

	// other services are unchanged
	reg, err = h.generateNomadServiceRegistration(workload.Services[0], workload)
	must.NoError(t, err)
	must.Eq(t, 23098, reg.Port)
	must.MapEmpty(t, reg.Meta)
}
