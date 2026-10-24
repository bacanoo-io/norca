// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"fmt"
	"net/http"
	"slices"
	"sort"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/go-bexpr"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/version"
)

// instance is a service registration with its node and Consul name.
type instance struct {
	reg        *structs.ServiceRegistration
	node       *structs.Node
	consulName string
}

// lookup returns the instances of the service of the given Consul name that
// match the tag, datacenter and node meta filters of r. Blocking queries wait
// on the service registrations.
func (s *Server) lookup(r *request, consulName string) ([]*instance, structs.QueryMeta, error) {
	ns, name := s.nomadName(consulName)
	args := &structs.ServiceRegistrationByNameRequest{
		ServiceName:  name,
		QueryOptions: r.queryOptions(ns, true),
	}
	if local := s.backend.Topology(); r.near && local.Region == r.region {
		args.Near = &local
		// the topology tiers count the instances that pass
		args.HealthyOnly = r.passing
	}

	var reply structs.ServiceRegistrationByNameResponse
	if err := s.backend.RPC(structs.ServiceRegistrationGetServiceRPCMethod, args, &reply); err != nil {
		return nil, reply.QueryMeta, err
	}
	return s.instances(r, consulName, reply.Services), reply.QueryMeta, nil
}

// instances returns the registrations that match the filters of r, with
// their nodes.
func (s *Server) instances(r *request, consulName string, regs []*structs.ServiceRegistration) []*instance {
	out := make([]*instance, 0, len(regs))
	for _, reg := range regs {
		if r.datacenter != "" && reg.Datacenter != r.datacenter {
			continue
		}
		if !hasTags(reg.Tags, r.tags) {
			continue
		}
		node := externalNode
		if reg.NodeID != "" {
			node = s.nodes.get(r.region, reg.NodeID)
		}
		if !nodeMetaMatches(node, r.nodeMeta) {
			continue
		}
		out = append(out, &instance{reg: reg, node: node, consulName: consulName})
	}
	return out
}

func hasTags(have, want []string) bool {
	for _, tag := range want {
		if !slices.Contains(have, tag) {
			return false
		}
	}
	return true
}

// allInstances returns the instances of every service of the shown
// namespaces. Blocking queries wait on the service registrations.
func (s *Server) allInstances(r *request) ([]*instance, structs.QueryMeta, error) {
	var meta structs.QueryMeta
	var out []*instance

	for i, ns := range s.cfg.Namespaces {
		args := &structs.ServiceRegistrationListRequest{QueryOptions: r.queryOptions(ns, i == 0)}
		var reply structs.ServiceRegistrationListResponse
		if err := s.backend.RPC(structs.ServiceRegistrationListRPCMethod, args, &reply); err != nil {
			return nil, meta, err
		}
		if i == 0 {
			meta = reply.QueryMeta
		}

		for _, stub := range reply.Services {
			for _, svc := range stub.Services {
				gargs := &structs.ServiceRegistrationByNameRequest{
					ServiceName:  svc.ServiceName,
					QueryOptions: r.queryOptions(ns, false),
				}
				gargs.AllowStale = true
				var greply structs.ServiceRegistrationByNameResponse
				if err := s.backend.RPC(structs.ServiceRegistrationGetServiceRPCMethod, gargs, &greply); err != nil {
					return nil, meta, err
				}
				out = append(out, s.instances(r, s.consulName(ns, svc.ServiceName), greply.Services)...)
			}
		}
	}
	return out, meta, nil
}

// dcOf returns the Consul datacenter of an instance.
func (s *Server) dcOf(r *request, reg *structs.ServiceRegistration) string {
	if s.cfg.DatacenterIsRegion {
		return r.dc
	}
	return reg.Datacenter
}

// agentService converts an instance into a Consul service.
func (s *Server) agentService(r *request, inst *instance) *consulapi.AgentService {
	reg := inst.reg
	tags := slices.Clone(reg.Tags)
	if tags == nil {
		tags = []string{}
	}
	meta := map[string]string{"external-source": "nomad"}
	if reg.External != nil && reg.External.Source == structs.ExternalServiceSourceConsul {
		// registered through the Consul agent API
		meta = map[string]string{}
	}
	for k, v := range reg.Meta {
		meta[k] = v
	}
	return &consulapi.AgentService{
		ID:          localID(reg),
		Service:     inst.consulName,
		Tags:        tags,
		Meta:        meta,
		Port:        reg.Port,
		Address:     reg.Address,
		Weights:     consulapi.AgentWeights{Passing: 1, Warning: 1},
		Datacenter:  s.dcOf(r, reg),
		CreateIndex: reg.CreateIndex,
		ModifyIndex: reg.ModifyIndex,
	}
}

// instanceHealth is the aggregate health of an instance as a Consul check
// status: critical while it is not ready.
func instanceHealth(reg *structs.ServiceRegistration) string {
	switch {
	case !reg.Ready:
		return consulapi.HealthCritical
	case reg.Health == "":
		return consulapi.HealthPassing
	default:
		return reg.Health
	}
}

// serviceChecks returns the Consul checks of an instance: its aggregate
// health, which includes the startup check of the workload, then one check
// per catalog check.
func (s *Server) serviceChecks(inst *instance) consulapi.HealthChecks {
	reg := inst.reg
	tags := slices.Clone(reg.Tags)
	if tags == nil {
		tags = []string{}
	}
	check := func(id, name, status, notes, output string) *consulapi.HealthCheck {
		return &consulapi.HealthCheck{
			Node:        inst.node.Name,
			CheckID:     id,
			Name:        name,
			Status:      status,
			Notes:       notes,
			Output:      output,
			ServiceID:   localID(reg),
			ServiceName: inst.consulName,
			ServiceTags: tags,
			Type:        "nomad",
			CreateIndex: reg.CreateIndex,
			ModifyIndex: reg.ModifyIndex,
		}
	}

	// external services report their own checks, as registered
	if reg.External != nil {
		checks := consulapi.HealthChecks{}
		for _, def := range reg.External.Checks {
			status, output := consulapi.HealthCritical, ""
			if state := reg.CheckState(def.ID); state != nil {
				status, output = state.Status, state.Output
			}
			hc := check(def.ID, def.Name, status, def.Notes, output)
			hc.Type = def.Type
			checks = append(checks, hc)
		}
		return checks
	}

	checks := consulapi.HealthChecks{check("nomad-health:"+reg.ID, "Nomad service health", instanceHealth(reg),
		"Readiness of the instance as determined by its Nomad checks", "")}
	for i, c := range reg.Checks {
		notes := ""
		if c.Role != "" {
			notes = "role: " + c.Role
		}
		checks = append(checks, check(fmt.Sprintf("service:%s:%d", reg.ID, i+1), c.Name, c.Status, notes, c.Output))
	}
	return checks
}

// serviceEntry converts an instance into a Consul health entry.
func (s *Server) serviceEntry(r *request, inst *instance) *consulapi.ServiceEntry {
	checks := consulapi.HealthChecks{serfHealth(inst.node)}
	checks = append(checks, s.serviceChecks(inst)...)
	return &consulapi.ServiceEntry{
		Node:    consulNode(inst.node, s.dcOf(r, inst.reg)),
		Service: s.agentService(r, inst),
		Checks:  checks,
	}
}

// applyFilter keeps the items of a slice matching the bexpr filter of r.
func applyFilter[T any](r *request, items []T) ([]T, error) {
	if r.filter == "" {
		return items, nil
	}
	eval, err := bexpr.CreateEvaluator(r.filter)
	if err != nil {
		return nil, badRequest("Failed to create boolean expression evaluator: %v", err)
	}
	out := make([]T, 0, len(items))
	for _, item := range items {
		ok, err := eval.Evaluate(item)
		if err != nil {
			return nil, badRequest("Failed to evaluate filter: %v", err)
		}
		if ok {
			out = append(out, item)
		}
	}
	return out, nil
}

// agentSelf serves GET /v1/agent/self with the data of the local node.
func (s *Server) agentSelf(_ http.ResponseWriter, r *request, _ string) (any, error) {
	node := s.backend.LocalNode()
	if node == nil {
		node = &structs.Node{}
	}
	dc := s.backend.Region()
	if !s.cfg.DatacenterIsRegion {
		dc = node.Datacenter
	}
	addr := nodeAddress(node)
	meta := make(map[string]string, len(node.Meta))
	for k, v := range node.Meta {
		meta[k] = v
	}
	return map[string]any{
		"Config": map[string]any{
			"Datacenter":        dc,
			"PrimaryDatacenter": dc,
			"NodeName":          node.Name,
			"NodeID":            node.ID,
			"Server":            false,
			"Revision":          version.GitCommit,
			"Version":           version.NorcaVersion,
			"Product":           "norca",
		},
		"Member": map[string]any{
			"Name":   node.Name,
			"Addr":   addr,
			"Port":   0,
			"Tags":   map[string]string{"dc": dc, "role": "node", "id": node.ID},
			"Status": 1,
		},
		"Meta":  meta,
		"Stats": map[string]any{},
	}, nil
}

// statusLeader serves GET /v1/status/leader.
func (s *Server) statusLeader(_ http.ResponseWriter, r *request, _ string) (any, error) {
	args := &structs.GenericRequest{QueryOptions: r.queryOptions("", false)}
	var leader string
	if err := s.backend.RPC("Status.Leader", args, &leader); err != nil {
		return nil, err
	}
	return leader, nil
}

// statusPeers serves GET /v1/status/peers.
func (s *Server) statusPeers(_ http.ResponseWriter, r *request, _ string) (any, error) {
	args := &structs.GenericRequest{QueryOptions: r.queryOptions("", false)}
	var peers []string
	if err := s.backend.RPC("Status.Peers", args, &peers); err != nil {
		return nil, err
	}
	if peers == nil {
		peers = []string{}
	}
	return peers, nil
}

// catalogDatacenters serves GET /v1/catalog/datacenters: the regions of the
// federation, or the datacenters of the local region.
func (s *Server) catalogDatacenters(_ http.ResponseWriter, r *request, _ string) (any, error) {
	if s.cfg.DatacenterIsRegion {
		args := &structs.GenericRequest{QueryOptions: r.queryOptions("", false)}
		var regions []string
		if err := s.backend.RPC("Region.List", args, &regions); err != nil {
			return nil, err
		}
		sort.Strings(regions)
		return regions, nil
	}

	dcs := map[string]bool{s.backend.Topology().Datacenter: true}
	args := &structs.NodeListRequest{QueryOptions: r.queryOptions("", false)}
	var reply structs.NodeListResponse
	if err := s.backend.RPC("Node.List", args, &reply); err == nil {
		for _, n := range reply.Nodes {
			dcs[n.Datacenter] = true
		}
	}
	out := make([]string, 0, len(dcs))
	for dc := range dcs {
		out = append(out, dc)
	}
	sort.Strings(out)
	return out, nil
}

// catalogServices serves GET /v1/catalog/services: the names of the services
// and their tags.
func (s *Server) catalogServices(w http.ResponseWriter, r *request, _ string) (any, error) {
	out := make(map[string][]string)
	add := func(name string, tags []string) {
		set := out[name]
		if set == nil {
			set = []string{}
		}
		for _, tag := range tags {
			if !slices.Contains(set, tag) {
				set = append(set, tag)
			}
		}
		out[name] = set
	}

	if r.nodeMeta != nil || r.datacenter != "" || r.filter != "" {
		instances, meta, err := s.allInstances(r)
		if err != nil {
			return nil, err
		}
		services := make([]*consulapi.CatalogService, 0, len(instances))
		for _, inst := range instances {
			services = append(services, s.catalogService1(r, inst))
		}
		if services, err = applyFilter(r, services); err != nil {
			return nil, err
		}
		for _, svc := range services {
			add(svc.ServiceName, svc.ServiceTags)
		}
		setMeta(w, r, meta)
	} else {
		var meta structs.QueryMeta
		for i, ns := range s.cfg.Namespaces {
			args := &structs.ServiceRegistrationListRequest{QueryOptions: r.queryOptions(ns, i == 0)}
			var reply structs.ServiceRegistrationListResponse
			if err := s.backend.RPC(structs.ServiceRegistrationListRPCMethod, args, &reply); err != nil {
				return nil, err
			}
			if i == 0 {
				meta = reply.QueryMeta
			}
			for _, stub := range reply.Services {
				for _, svc := range stub.Services {
					add(s.consulName(ns, svc.ServiceName), svc.Tags)
				}
			}
		}
		setMeta(w, r, meta)
	}

	for _, tags := range out {
		sort.Strings(tags)
	}
	return out, nil
}

// catalogService1 converts an instance into a Consul catalog service.
func (s *Server) catalogService1(r *request, inst *instance) *consulapi.CatalogService {
	node := consulNode(inst.node, s.dcOf(r, inst.reg))
	svc := s.agentService(r, inst)
	return &consulapi.CatalogService{
		ID:                       node.ID,
		Node:                     node.Node,
		Address:                  node.Address,
		Datacenter:               node.Datacenter,
		TaggedAddresses:          node.TaggedAddresses,
		NodeMeta:                 node.Meta,
		ServiceID:                svc.ID,
		ServiceName:              svc.Service,
		ServiceAddress:           svc.Address,
		ServiceTags:              svc.Tags,
		ServiceMeta:              svc.Meta,
		ServicePort:              svc.Port,
		ServiceWeights:           consulapi.Weights{Passing: svc.Weights.Passing, Warning: svc.Weights.Warning},
		ServiceEnableTagOverride: false,
		CreateIndex:              svc.CreateIndex,
		ModifyIndex:              svc.ModifyIndex,
	}
}

// catalogService serves GET /v1/catalog/service/:name.
func (s *Server) catalogService(w http.ResponseWriter, r *request, name string) (any, error) {
	instances, meta, err := s.lookup(r, name)
	if err != nil {
		return nil, err
	}
	out := make([]*consulapi.CatalogService, 0, len(instances))
	for _, inst := range instances {
		out = append(out, s.catalogService1(r, inst))
	}
	if out, err = applyFilter(r, out); err != nil {
		return nil, err
	}
	setMeta(w, r, meta)
	return out, nil
}

// catalogNodes serves GET /v1/catalog/nodes.
func (s *Server) catalogNodes(w http.ResponseWriter, r *request, _ string) (any, error) {
	args := &structs.NodeListRequest{QueryOptions: r.queryOptions("", true)}
	var reply structs.NodeListResponse
	if err := s.backend.RPC("Node.List", args, &reply); err != nil {
		return nil, err
	}

	out := make([]*consulapi.Node, 0, len(reply.Nodes))
	for _, stub := range reply.Nodes {
		if r.datacenter != "" && stub.Datacenter != r.datacenter {
			continue
		}
		node := s.nodes.get(r.region, stub.ID)
		if !nodeMetaMatches(node, r.nodeMeta) {
			continue
		}
		dc := r.dc
		if !s.cfg.DatacenterIsRegion {
			dc = stub.Datacenter
		}
		out = append(out, consulNode(node, dc))
	}
	out, err := applyFilter(r, out)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	setMeta(w, r, reply.QueryMeta)
	return out, nil
}

// catalogNode serves GET /v1/catalog/node/:node: the node of the given name
// and the instances running on it.
func (s *Server) catalogNode(w http.ResponseWriter, r *request, name string) (any, error) {
	args := &structs.NodeListRequest{QueryOptions: r.queryOptions("", false)}
	var reply structs.NodeListResponse
	if err := s.backend.RPC("Node.List", args, &reply); err != nil {
		return nil, err
	}
	var id string
	for _, stub := range reply.Nodes {
		if stub.Name == name || stub.ID == name {
			id = stub.ID
			break
		}
	}

	instances, meta, err := s.allInstances(r)
	if err != nil {
		return nil, err
	}
	setMeta(w, r, meta)
	if id == "" {
		return nil, nil
	}

	node := s.nodes.get(r.region, id)
	out := &consulapi.CatalogNode{
		Node:     consulNode(node, r.dc),
		Services: make(map[string]*consulapi.AgentService),
	}
	if !s.cfg.DatacenterIsRegion {
		out.Node.Datacenter = node.Datacenter
	}
	for _, inst := range instances {
		if inst.reg.NodeID == id {
			out.Services[localID(inst.reg)] = s.agentService(r, inst)
		}
	}
	return out, nil
}

// healthService serves GET /v1/health/service/:name.
func (s *Server) healthService(w http.ResponseWriter, r *request, name string) (any, error) {
	instances, meta, err := s.lookup(r, name)
	if err != nil {
		return nil, err
	}
	out := make([]*consulapi.ServiceEntry, 0, len(instances))
	for _, inst := range instances {
		entry := s.serviceEntry(r, inst)
		if r.passing && entry.Checks.AggregatedStatus() != consulapi.HealthPassing {
			continue
		}
		out = append(out, entry)
	}
	if out, err = applyFilter(r, out); err != nil {
		return nil, err
	}
	setMeta(w, r, meta)
	return out, nil
}

// healthChecks serves GET /v1/health/checks/:name: the checks of the
// instances of the service.
func (s *Server) healthChecks(w http.ResponseWriter, r *request, name string) (any, error) {
	instances, meta, err := s.lookup(r, name)
	if err != nil {
		return nil, err
	}
	out := make([]*consulapi.HealthCheck, 0, len(instances))
	for _, inst := range instances {
		out = append(out, s.serviceChecks(inst)...)
	}
	if out, err = applyFilter(r, out); err != nil {
		return nil, err
	}
	setMeta(w, r, meta)
	return out, nil
}

// healthState serves GET /v1/health/state/:state: the checks in the given
// state (any, passing, warning, critical), including those of the nodes.
func (s *Server) healthState(w http.ResponseWriter, r *request, state string) (any, error) {
	switch state {
	case consulapi.HealthAny, consulapi.HealthPassing, consulapi.HealthWarning, consulapi.HealthCritical:
	default:
		return nil, badRequest("Bad state %q", state)
	}

	instances, meta, err := s.allInstances(r)
	if err != nil {
		return nil, err
	}

	var all consulapi.HealthChecks
	seenNodes := make(map[string]bool)
	for _, inst := range instances {
		if !seenNodes[inst.node.ID] {
			seenNodes[inst.node.ID] = true
			all = append(all, serfHealth(inst.node))
		}
		all = append(all, s.serviceChecks(inst)...)
	}

	out := make([]*consulapi.HealthCheck, 0, len(all))
	for _, check := range all {
		if state == consulapi.HealthAny || check.Status == state {
			out = append(out, check)
		}
	}
	if out, err = applyFilter(r, out); err != nil {
		return nil, err
	}
	setMeta(w, r, meta)
	return out, nil
}

// healthConnect serves GET /v1/health/connect/:name. There are no Connect
// proxies in the native catalog, so the answer is always empty; it blocks
// like the other service endpoints, so watchers do not spin.
func (s *Server) healthConnect(w http.ResponseWriter, r *request, name string) (any, error) {
	_, meta, err := s.lookup(r, name)
	if err != nil {
		return nil, err
	}
	setMeta(w, r, meta)
	return []*consulapi.ServiceEntry{}, nil
}
