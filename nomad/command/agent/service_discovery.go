// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

// builtinHTTPAddr is the address of the HTTP server serving the workloads of
// the client (templates and the Task API).
const builtinHTTPAddr = "builtin"

// fromWorkload returns whether the request was made by a workload of the
// client, through the template transport or the Task API.
func (s *HTTPServer) fromWorkload() bool {
	return s.Addr == builtinHTTPAddr
}

// parseHealthyOnly parses the healthy query parameter of service lookups.
// Without it, lookups made by workloads use the default_healthy_only setting
// of the client, and other lookups return every instance.
func (s *HTTPServer) parseHealthyOnly(req *http.Request) (bool, error) {
	if raw := req.URL.Query().Get("healthy"); raw != "" {
		healthy, err := strconv.ParseBool(raw)
		if err != nil {
			return false, CodedError(http.StatusBadRequest, "invalid healthy value: "+raw)
		}
		return healthy, nil
	}
	if !s.fromWorkload() {
		return false, nil
	}
	if c := s.agent.Client(); c != nil {
		return c.GetConfig().NativeServiceDiscovery.HealthyOnly(), nil
	}
	return false, nil
}

// apiServiceDiscoveryToStructs converts the discovery block of a service.
func apiServiceDiscoveryToStructs(in *api.ServiceDiscovery) *structs.ServiceDiscovery {
	if in == nil {
		return nil
	}
	out := &structs.ServiceDiscovery{
		Prefer:           in.Prefer,
		MinLocal:         in.MinLocal,
		OverflowFraction: structs.DefaultDiscoveryOverflowFraction,
		Failover:         slices.Clone(in.Failover),
		IncludeNotReady:  in.IncludeNotReady,
	}
	if in.OverflowFraction != nil {
		out.OverflowFraction = *in.OverflowFraction
	}
	return out
}
