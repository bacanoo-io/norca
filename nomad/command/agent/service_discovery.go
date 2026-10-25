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

// parseNear parses the near query parameter of service lookups. "self" (or
// "_agent", as in Consul) orders and selects the instances by their topology
// relative to the client agent, following the discovery block of the
// service. Lookups made by workloads are topology-aware by default; near=none
// turns it off.
func (s *HTTPServer) parseNear(req *http.Request) (*structs.Topology, error) {
	near := req.URL.Query().Get("near")
	switch near {
	case "":
		if !s.fromWorkload() {
			return nil, nil
		}
	case "none":
		return nil, nil
	case "self", "_agent":
	default:
		return nil, CodedError(http.StatusBadRequest, `near must be "self" or "none"`)
	}

	c := s.agent.Client()
	if c == nil {
		if near == "" {
			return nil, nil
		}
		return nil, CodedError(http.StatusBadRequest, "near=self requires a client agent")
	}
	topology := c.Topology()
	return &topology, nil
}

// serviceFailover performs the regional failover of a topology-aware service
// lookup: if the local region has no instance left after filtering, the
// regions of the failover list of the service are queried in order, and the
// instances of the first one with any are returned.
//
// The failover queries do not block and the index of the local region is
// kept, so blocking callers wake up on local changes.
func (s *HTTPServer) serviceFailover(args *structs.ServiceRegistrationByNameRequest,
	reply *structs.ServiceRegistrationByNameResponse) {

	if args.Near == nil || len(reply.Services) > 0 || reply.Discovery == nil ||
		(args.Region != "" && args.Region != args.Near.Region) {
		return
	}

	for _, region := range reply.Discovery.Failover {
		if region == args.Near.Region {
			continue
		}
		fargs := *args
		fargs.Region = region
		fargs.MinQueryIndex = 0
		fargs.Near = nil
		fargs.NextToken = ""
		var freply structs.ServiceRegistrationByNameResponse
		if err := s.agent.RPC(structs.ServiceRegistrationGetServiceRPCMethod, &fargs, &freply); err != nil {
			s.logger.Debug("service failover lookup failed", "service", args.ServiceName, "region", region, "error", err)
			continue
		}
		if len(freply.Services) > 0 {
			reply.Services = freply.Services
			return
		}
	}
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
