// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

// externalServiceRequest serves the external service endpoints below
// /v1/service/external/:
//
//	PUT    /v1/service/external/:name     register or update
//	PUT    /v1/service/external/:id/ttl   renew or update the TTL checks
//	DELETE /v1/service/external/:id       deregister
//
// handled is false for requests that are not for these endpoints, such as
// the deletion of a registration of a service named "external".
func (s *HTTPServer) externalServiceRequest(resp http.ResponseWriter, req *http.Request,
	rest string) (out any, handled bool, err error) {

	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 1 && parts[0] != "" && req.Method == http.MethodPut:
		out, err := s.externalServiceRegister(resp, req, parts[0])
		return out, true, err

	case len(parts) == 2 && parts[0] != "" && parts[1] == "ttl" &&
		(req.Method == http.MethodPut || req.Method == http.MethodPost):
		out, err := s.externalServiceTTL(resp, req, parts[0])
		return out, true, err

	case len(parts) == 1 && parts[0] != "" && req.Method == http.MethodDelete:
		args := structs.ExternalServiceDeleteRequest{ID: parts[0]}
		s.parseWriteRequest(req, &args.WriteRequest)
		var reply structs.ExternalServiceDeleteResponse
		if err := s.agent.RPC(structs.ServiceRegistrationDeleteExternalRPCMethod, &args, &reply); err != nil {
			if strings.Contains(err.Error(), "is not an external service") {
				// a registration of a service named "external"
				return nil, false, nil
			}
			return nil, true, err
		}
		setIndex(resp, reply.Index)
		return nil, true, nil
	}
	return nil, false, nil
}

// externalServiceRegister serves PUT /v1/service/external/:name.
func (s *HTTPServer) externalServiceRegister(resp http.ResponseWriter, req *http.Request,
	name string) (any, error) {

	var in api.ExternalService
	if err := decodeBody(req, &in); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if in.Name == "" {
		in.Name = name
	}
	if in.Name != name {
		return nil, CodedError(http.StatusBadRequest, "service name does not match the URL")
	}

	reg, err := ApiExternalServiceToStructs(&in)
	if err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	args := structs.ExternalServiceUpsertRequest{Service: reg}
	s.parseWriteRequest(req, &args.WriteRequest)

	var reply structs.ExternalServiceUpsertResponse
	if client := s.agent.Client(); client != nil && reg.External.HasNodeChecks() {
		// the client node of this agent runs the http and tcp checks
		err = client.RegisterExternalService(&args, &reply)
	} else {
		err = s.agent.RPC(structs.ServiceRegistrationUpsertExternalRPCMethod, &args, &reply)
	}
	if err != nil {
		return nil, err
	}
	setIndex(resp, reply.Index)
	return &api.ExternalServiceRegisterResponse{ID: reply.ID, Index: reply.Index}, nil
}

// externalServiceTTL serves PUT /v1/service/external/:id/ttl. The update is
// read from the body, or from the status, output and check query parameters.
func (s *HTTPServer) externalServiceTTL(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	var in api.ExternalCheckUpdate
	if req.Body != nil && req.Body != http.NoBody {
		if err := decodeBody(req, &in); err != nil && err != io.EOF {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}
	}
	q := req.URL.Query()
	if v := q.Get("status"); v != "" {
		in.Status = v
	}
	if v := q.Get("output"); v != "" {
		in.Output = v
	}
	if v := q.Get("check"); v != "" {
		in.CheckID = v
	}
	if in.Status == "" {
		in.Status = structs.ServiceHealthPassing
	}

	args := structs.ExternalServiceCheckUpdateRequest{
		ID:      id,
		Updates: []*structs.ExternalCheckUpdate{{CheckID: in.CheckID, Status: in.Status, Output: in.Output}},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var reply structs.ExternalServiceCheckUpdateResponse
	if err := s.agent.RPC(structs.ServiceRegistrationUpdateExternalChecksRPCMethod, &args, &reply); err != nil {
		return nil, err
	}
	setIndex(resp, reply.Index)
	return nil, nil
}

// ExternalServiceListRequest serves GET /v1/services/external.
func (s *HTTPServer) ExternalServiceListRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.ExternalServiceListRequest{NodeID: req.URL.Query().Get("node_id")}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var reply structs.ExternalServiceListResponse
	if err := s.agent.RPC(structs.ServiceRegistrationListExternalRPCMethod, &args, &reply); err != nil {
		return nil, err
	}
	setMeta(resp, &reply.QueryMeta)
	if reply.Services == nil {
		reply.Services = make([]*structs.ServiceRegistration, 0)
	}
	return reply.Services, nil
}

// ApiExternalServiceToStructs converts an external service of the native API
// into a registration, with the defaults of the native API: kind address,
// check IDs from their types, TTL checks passing at registration, and
// deregistration after DefaultExternalDeregisterCriticalAfter critical.
func ApiExternalServiceToStructs(in *api.ExternalService) (*structs.ServiceRegistration, error) {
	kind := in.Kind
	if kind == "" {
		kind = structs.ExternalServiceKindAddress
		if in.Target != "" && in.Address == "" {
			kind = structs.ExternalServiceKindAlias
		}
	}
	address := in.Address
	if kind == structs.ExternalServiceKindAlias {
		if in.Target != "" && in.Address != "" && in.Target != in.Address {
			return nil, fmt.Errorf("an alias has a target, not an address")
		}
		if in.Target != "" {
			address = in.Target
		}
	}

	id := in.ID
	if id == "" {
		id = in.Name
	}

	ext := &structs.ServiceExternal{
		Kind:                    kind,
		Source:                  structs.ExternalServiceSourceAPI,
		DeregisterCriticalAfter: structs.DefaultExternalDeregisterCriticalAfter,
	}
	if in.DeregisterCriticalAfter != nil {
		ext.DeregisterCriticalAfter = *in.DeregisterCriticalAfter
	}

	used := make(map[string]int)
	for _, c := range in.Checks {
		if c == nil {
			continue
		}
		check := &structs.ExternalServiceCheck{
			ID:            c.ID,
			Name:          c.Name,
			Type:          strings.ToLower(c.Type),
			Notes:         c.Notes,
			TTL:           c.TTL,
			HTTP:          c.HTTP,
			Method:        c.Method,
			Header:        c.Header,
			Body:          c.Body,
			TLSSkipVerify: c.TLSSkipVerify,
			TLSServerName: c.TLSServerName,
			TCP:           c.TCP,
			Interval:      c.Interval,
			Timeout:       c.Timeout,
			InitialStatus: c.InitialStatus,
		}
		if check.ID == "" {
			used[check.Type]++
			check.ID = check.Type
			if n := used[check.Type]; n > 1 {
				check.ID = fmt.Sprintf("%s:%d", check.Type, n)
			}
		}
		if check.Name == "" {
			check.Name = fmt.Sprintf("%s %s check", in.Name, check.Type)
		}
		if check.InitialStatus == "" {
			check.InitialStatus = structs.ServiceHealthPassing
		}
		ext.Checks = append(ext.Checks, check)
	}

	return &structs.ServiceRegistration{
		ID:          id,
		ServiceName: in.Name,
		Datacenter:  in.Datacenter,
		Tags:        in.Tags,
		Address:     address,
		Port:        in.Port,
		Meta:        in.Meta,
		External:    ext,
	}, nil
}
