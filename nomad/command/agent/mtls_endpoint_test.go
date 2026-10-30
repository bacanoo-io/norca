// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestHTTP_ServiceMTLSConversion(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, apiServiceMTLSToStructs(nil))
	must.Eq(t, &structs.ServiceMTLS{
		Mode: structs.MTLSModeApp, ChangeMode: structs.MTLSChangeModeSignal, ChangeSignal: "SIGHUP", ChangeOn: structs.MTLSChangeOnBundle,
	}, apiServiceMTLSToStructs(&api.ServiceMTLS{
		Mode: api.MTLSModeApp, ChangeMode: api.MTLSChangeModeSignal, ChangeSignal: "SIGHUP", ChangeOn: api.MTLSChangeOnBundle,
	}))
	got := apiServiceMTLSToStructs(&api.ServiceMTLS{
		Mode: api.MTLSModeSidecar,
		Upstreams: []*api.ServiceMTLSUpstream{
			nil,
			{Name: "db", Service: "postgres", LocalPort: 5432},
			{Name: "cache", Service: "redis", Namespace: "cache", Region: "eu", Gateway: "edge", LocalPort: 6379},
		},
	})
	must.Eq(t, &structs.ServiceMTLS{
		Mode: structs.MTLSModeSidecar,
		Upstreams: []*structs.ServiceMTLSUpstream{
			{Name: "db", Service: "postgres", LocalPort: 5432},
			{Name: "cache", Service: "redis", Namespace: "cache", Region: "eu", Gateway: "edge", LocalPort: 6379},
		},
	}, got)
}
