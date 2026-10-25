// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

// LocalServices gives the API the external services owned by the local node:
// the services registered through the agent endpoints, as a Consul agent
// serves its local services.
type LocalServices interface {
	// Services returns the registrations owned by the local node.
	Services() []*structs.ServiceRegistration

	// Refresh re-reads them from the servers.
	Refresh() error
}

// maxRegisterBody bounds the size of a registration body.
const maxRegisterBody = 1 << 20

// localServices returns the external services owned by the local node.
func (s *Server) localServices() (LocalServices, error) {
	local := s.backend.LocalServices()
	if local == nil {
		return nil, &codedError{http.StatusServiceUnavailable, "the agent is starting"}
	}
	return local, nil
}

// agentRegistrationID returns the ID of the registration of a service
// registered at the local agent under the given ID: service IDs are unique
// per Consul agent, registration IDs per namespace.
func agentRegistrationID(nodeID, localID string) string {
	return "_consul-agent-" + nodeID + "-" + localID
}

// localID returns the ID of a registration as the Consul API shows it.
func localID(reg *structs.ServiceRegistration) string {
	if reg.External != nil && reg.External.LocalID != "" {
		return reg.External.LocalID
	}
	return reg.ID
}

// findLocal returns the local registration matching match, re-reading the
// local registrations once if it is not found.
func (s *Server) findLocal(match func(*structs.ServiceRegistration) bool) (*structs.ServiceRegistration, error) {
	local, err := s.localServices()
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		for _, reg := range local.Services() {
			if match(reg) {
				return reg, nil
			}
		}
		if attempt == 0 {
			if err := local.Refresh(); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

// writeOptions returns the Nomad write request of r for namespace. Writes go
// to the region of the client, as Consul agents write to their datacenter.
func (s *Server) writeOptions(r *request, namespace string) structs.WriteRequest {
	return structs.WriteRequest{
		Region:    s.backend.Region(),
		Namespace: namespace,
		AuthToken: r.token,
	}
}

// parseDuration parses a Consul duration, a Go duration string.
func parseDuration(field, raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, badRequest("Invalid %s %q: %v", field, raw, err)
	}
	return d, nil
}

// agentServiceRegister serves PUT /v1/agent/service/register: it registers an
// external service owned by the local node. TTL checks are renewed through
// the check endpoints; http and tcp checks run on this node.
func (s *Server) agentServiceRegister(_ http.ResponseWriter, r *request, _ string) (any, error) {
	var in consulapi.AgentServiceRegistration
	body, err := io.ReadAll(io.LimitReader(r.http.Body, maxRegisterBody))
	if err != nil {
		return nil, badRequest("Request decode failed: %v", err)
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, badRequest("Request decode failed: %v", err)
	}

	switch {
	case in.Name == "":
		return nil, badRequest("Missing service name")
	case in.Kind != "" && in.Kind != consulapi.ServiceKindTypical:
		return nil, badRequest("Service kind %q is not supported by Norca: there is no Consul Connect", in.Kind)
	case in.Proxy != nil || (in.Connect != nil && (in.Connect.Native || in.Connect.SidecarService != nil)):
		return nil, badRequest("Consul Connect is not supported by Norca")
	}

	node := s.backend.LocalNode()
	if node == nil {
		return nil, &codedError{http.StatusServiceUnavailable, "the agent is starting"}
	}

	namespace, name := s.nomadName(in.Name)
	id := in.ID
	if id == "" {
		id = in.Name
	}
	address := in.Address
	if address == "" {
		address = nodeAddress(node)
	}

	ext := &structs.ServiceExternal{
		Kind:    structs.ExternalServiceKindAddress,
		Source:  structs.ExternalServiceSourceConsul,
		LocalID: id,
	}
	checks := slices.Clone([]*consulapi.AgentServiceCheck(in.Checks))
	if in.Check != nil {
		checks = append([]*consulapi.AgentServiceCheck{in.Check}, checks...)
	}
	for i, c := range checks {
		if c == nil {
			continue
		}
		check, dca, err := convertCheck(c, in.Name, id, i, len(checks))
		if err != nil {
			return nil, err
		}
		if dca > 0 && (ext.DeregisterCriticalAfter == 0 || dca < ext.DeregisterCriticalAfter) {
			ext.DeregisterCriticalAfter = dca
		}
		ext.Checks = append(ext.Checks, check)
	}

	reg := &structs.ServiceRegistration{
		ID:          agentRegistrationID(node.ID, id),
		ServiceName: name,
		Tags:        in.Tags,
		Address:     address,
		Port:        in.Port,
		Meta:        in.Meta,
		External:    ext,
	}
	if len(reg.Meta) == 0 {
		reg.Meta = nil
	}
	args := &structs.ExternalServiceUpsertRequest{
		Service:      reg,
		NodeSecretID: s.backend.NodeSecret(),
		WriteRequest: s.writeOptions(r, namespace),
	}
	var reply structs.ExternalServiceUpsertResponse
	if err := s.backend.RPC(structs.ServiceRegistrationUpsertExternalRPCMethod, args, &reply); err != nil {
		return nil, err
	}
	if local := s.backend.LocalServices(); local != nil {
		if err := local.Refresh(); err != nil {
			s.logger.Debug("failed to refresh the local services", "error", err)
		}
	}
	return nil, nil
}

// convertCheck converts the i-th of n checks of a service registered at the
// agent. It returns the check and its DeregisterCriticalServiceAfter.
func convertCheck(c *consulapi.AgentServiceCheck, service, serviceID string, i, n int) (
	*structs.ExternalServiceCheck, time.Duration, error) {

	check := &structs.ExternalServiceCheck{
		ID:            c.CheckID,
		Name:          c.Name,
		Notes:         c.Notes,
		HTTP:          c.HTTP,
		Method:        c.Method,
		Header:        c.Header,
		Body:          c.Body,
		TLSSkipVerify: c.TLSSkipVerify,
		TLSServerName: c.TLSServerName,
		TCP:           c.TCP,
		InitialStatus: c.Status,
	}
	switch {
	case c.TTL != "":
		check.Type = structs.ExternalCheckTypeTTL
	case c.HTTP != "":
		check.Type = structs.ExternalCheckTypeHTTP
	case c.TCP != "" && c.TCPUseTLS:
		return nil, 0, badRequest("TCP checks with TLS are not supported by Norca")
	case c.TCP != "":
		check.Type = structs.ExternalCheckTypeTCP
	case len(c.Args) > 0 || c.DockerContainerID != "" || c.Shell != "":
		return nil, 0, badRequest("Script checks are not supported by Norca (ttl, http and tcp checks are)")
	case c.GRPC != "" || c.H2PING != "" || c.UDP != "" || c.AliasService != "" || c.AliasNode != "":
		return nil, 0, badRequest("This check type is not supported by Norca (ttl, http and tcp checks are)")
	default:
		return nil, 0, badRequest("Invalid check: one of TTL, HTTP or TCP must be set")
	}

	var err error
	if check.TTL, err = parseDuration("TTL", c.TTL); err != nil {
		return nil, 0, err
	}
	if check.Interval, err = parseDuration("Interval", c.Interval); err != nil {
		return nil, 0, err
	}
	if check.Timeout, err = parseDuration("Timeout", c.Timeout); err != nil {
		return nil, 0, err
	}
	dca, err := parseDuration("DeregisterCriticalServiceAfter", c.DeregisterCriticalServiceAfter)
	if err != nil {
		return nil, 0, err
	}

	// the IDs and names Consul gives checks defined with a service
	if check.ID == "" {
		check.ID = "service:" + serviceID
		if n > 1 {
			check.ID = fmt.Sprintf("service:%s:%d", serviceID, i+1)
		}
	}
	if check.Name == "" {
		check.Name = fmt.Sprintf("Service '%s' check", service)
	}
	if check.InitialStatus == "" {
		check.InitialStatus = consulapi.HealthCritical
	}
	return check, dca, nil
}

// agentServiceDeregister serves PUT /v1/agent/service/deregister/:id.
func (s *Server) agentServiceDeregister(_ http.ResponseWriter, r *request, id string) (any, error) {
	reg, err := s.findLocal(func(reg *structs.ServiceRegistration) bool { return localID(reg) == id })
	if err != nil {
		return nil, err
	}
	if reg == nil {
		return nil, &codedError{http.StatusNotFound,
			fmt.Sprintf("Unknown service ID %q. Ensure that the service ID is passed, not the service name.", id)}
	}
	args := &structs.ExternalServiceDeleteRequest{ID: reg.ID, WriteRequest: s.writeOptions(r, reg.Namespace)}
	var reply structs.ExternalServiceDeleteResponse
	if err := s.backend.RPC(structs.ServiceRegistrationDeleteExternalRPCMethod, args, &reply); err != nil {
		return nil, err
	}
	if local := s.backend.LocalServices(); local != nil {
		_ = local.Refresh()
	}
	return nil, nil
}

// agentCheckTTL returns the handler of PUT /v1/agent/check/pass|warn|fail/:id,
// which sets a TTL check to status with the note parameter as output.
func (s *Server) agentCheckTTL(status string) handlerFunc {
	return func(_ http.ResponseWriter, r *request, checkID string) (any, error) {
		return nil, s.updateCheck(r, checkID, status, r.http.URL.Query().Get("note"))
	}
}

// agentCheckUpdate serves PUT /v1/agent/check/update/:id with a body of
// Status and Output.
func (s *Server) agentCheckUpdate(_ http.ResponseWriter, r *request, checkID string) (any, error) {
	var in struct {
		Status string
		Output string
	}
	if err := json.NewDecoder(io.LimitReader(r.http.Body, maxRegisterBody)).Decode(&in); err != nil {
		return nil, badRequest("Request decode failed: %v", err)
	}
	switch in.Status {
	case consulapi.HealthPassing, consulapi.HealthWarning, consulapi.HealthCritical:
	default:
		return nil, badRequest("Invalid check status: %q", in.Status)
	}
	return nil, s.updateCheck(r, checkID, in.Status, in.Output)
}

// updateCheck updates a TTL check of a local service.
func (s *Server) updateCheck(r *request, checkID, status, output string) error {
	reg, err := s.findLocal(func(reg *structs.ServiceRegistration) bool { return reg.External.Check(checkID) != nil })
	if err != nil {
		return err
	}
	if reg == nil {
		return &codedError{http.StatusNotFound, fmt.Sprintf("Unknown check ID %q. Ensure that the check ID is passed, not the check name.", checkID)}
	}
	args := &structs.ExternalServiceCheckUpdateRequest{
		ID:           reg.ID,
		Updates:      []*structs.ExternalCheckUpdate{{CheckID: checkID, Status: status, Output: output}},
		WriteRequest: s.writeOptions(r, reg.Namespace),
	}
	var reply structs.ExternalServiceCheckUpdateResponse
	if err := s.backend.RPC(structs.ServiceRegistrationUpdateExternalChecksRPCMethod, args, &reply); err != nil {
		return err
	}

	// a status change is written: show it at the agent right away
	if state := reg.CheckState(checkID); state == nil || state.Status != status {
		if local := s.backend.LocalServices(); local != nil {
			_ = local.Refresh()
		}
	}
	return nil
}

// localInstances returns the local services as instances on the local node.
func (s *Server) localInstances() ([]*instance, error) {
	local, err := s.localServices()
	if err != nil {
		return nil, err
	}
	node := s.backend.LocalNode()
	if node == nil {
		node = &structs.Node{}
	}
	var out []*instance
	for _, reg := range local.Services() {
		if !s.shownNamespace(reg.Namespace) {
			continue
		}
		out = append(out, &instance{reg: reg, node: node, consulName: s.consulName(reg.Namespace, reg.ServiceName)})
	}
	sort.Slice(out, func(i, j int) bool { return localID(out[i].reg) < localID(out[j].reg) })
	return out, nil
}

// shownNamespace returns whether the namespace is shown by the API.
func (s *Server) shownNamespace(namespace string) bool {
	return slices.Contains(s.cfg.Namespaces, namespace)
}

// agentServices serves GET /v1/agent/services: the services registered at
// the local agent.
func (s *Server) agentServices(_ http.ResponseWriter, r *request, _ string) (any, error) {
	instances, err := s.localInstances()
	if err != nil {
		return nil, err
	}
	services := make([]*consulapi.AgentService, 0, len(instances))
	for _, inst := range instances {
		services = append(services, s.agentService(r, inst))
	}
	if services, err = applyFilter(r, services); err != nil {
		return nil, err
	}
	out := make(map[string]*consulapi.AgentService, len(services))
	for _, svc := range services {
		out[svc.ID] = svc
	}
	return out, nil
}

// agentService1 serves GET /v1/agent/service/:id.
func (s *Server) agentService1(_ http.ResponseWriter, r *request, id string) (any, error) {
	instances, err := s.localInstances()
	if err != nil {
		return nil, err
	}
	for _, inst := range instances {
		if localID(inst.reg) == id {
			return s.agentService(r, inst), nil
		}
	}
	return nil, &codedError{http.StatusNotFound, fmt.Sprintf("unknown service ID: %s", id)}
}

// agentChecks serves GET /v1/agent/checks: the checks of the services
// registered at the local agent.
func (s *Server) agentChecks(_ http.ResponseWriter, r *request, _ string) (any, error) {
	instances, err := s.localInstances()
	if err != nil {
		return nil, err
	}
	var checks []*consulapi.AgentCheck
	for _, inst := range instances {
		for _, hc := range s.serviceChecks(inst) {
			checks = append(checks, &consulapi.AgentCheck{
				Node:        hc.Node,
				CheckID:     hc.CheckID,
				Name:        hc.Name,
				Status:      hc.Status,
				Notes:       hc.Notes,
				Output:      hc.Output,
				ServiceID:   hc.ServiceID,
				ServiceName: hc.ServiceName,
				Type:        hc.Type,
			})
		}
	}
	if checks, err = applyFilter(r, checks); err != nil {
		return nil, err
	}
	out := make(map[string]*consulapi.AgentCheck, len(checks))
	for _, c := range checks {
		out[c.CheckID] = c
	}
	return out, nil
}

// writeRoute returns the handler of a write endpoint and its path argument.
func (s *Server) writeRoute(path string) (handlerFunc, string, bool) {
	if path == "/v1/agent/service/register" {
		return s.agentServiceRegister, "", true
	}
	prefixed := []struct {
		prefix string
		h      handlerFunc
	}{
		{"/v1/agent/service/deregister/", s.agentServiceDeregister},
		{"/v1/agent/check/pass/", s.agentCheckTTL(consulapi.HealthPassing)},
		{"/v1/agent/check/warn/", s.agentCheckTTL(consulapi.HealthWarning)},
		{"/v1/agent/check/fail/", s.agentCheckTTL(consulapi.HealthCritical)},
		{"/v1/agent/check/update/", s.agentCheckUpdate},
	}
	for _, p := range prefixed {
		if arg, ok := strings.CutPrefix(path, p.prefix); ok && arg != "" {
			return p.h, arg, true
		}
	}
	return nil, "", false
}
