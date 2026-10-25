// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// NetworkPoliciesRequest handles /v1/network-policies.
func (s *HTTPServer) NetworkPoliciesRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	switch req.Method {
	case http.MethodGet:
		return s.networkPolicyList(resp, req)
	case http.MethodPut, http.MethodPost:
		return s.networkPolicyUpsert(resp, req, "")
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// NetworkPolicySpecificRequest handles /v1/network-policy/:name and
// /v1/network-policy/:name/status.
func (s *HTTPServer) NetworkPolicySpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	name := strings.TrimPrefix(req.URL.Path, "/v1/network-policy/")
	if status, ok := strings.CutSuffix(name, "/status"); ok {
		if req.Method != http.MethodGet {
			return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
		}
		return s.networkPolicyStatus(resp, req, status, false)
	}
	if name == "" {
		return nil, CodedError(http.StatusBadRequest, "missing network policy name")
	}

	switch req.Method {
	case http.MethodGet:
		return s.networkPolicyQuery(resp, req, name)
	case http.MethodPut, http.MethodPost:
		return s.networkPolicyUpsert(resp, req, name)
	case http.MethodDelete:
		return s.networkPolicyDelete(resp, req, name)
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// AdminNetworkPoliciesRequest handles /v1/admin-network-policies.
func (s *HTTPServer) AdminNetworkPoliciesRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	switch req.Method {
	case http.MethodGet:
		return s.adminNetworkPolicyList(resp, req)
	case http.MethodPut, http.MethodPost:
		return s.adminNetworkPolicyUpsert(resp, req, "")
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// AdminNetworkPolicySpecificRequest handles /v1/admin-network-policy/:name
// and /v1/admin-network-policy/:name/status.
func (s *HTTPServer) AdminNetworkPolicySpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	name := strings.TrimPrefix(req.URL.Path, "/v1/admin-network-policy/")
	if status, ok := strings.CutSuffix(name, "/status"); ok {
		if req.Method != http.MethodGet {
			return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
		}
		return s.networkPolicyStatus(resp, req, status, true)
	}
	if name == "" {
		return nil, CodedError(http.StatusBadRequest, "missing admin network policy name")
	}

	switch req.Method {
	case http.MethodGet:
		return s.adminNetworkPolicyQuery(resp, req, name)
	case http.MethodPut, http.MethodPost:
		return s.adminNetworkPolicyUpsert(resp, req, name)
	case http.MethodDelete:
		return s.adminNetworkPolicyDelete(resp, req, name)
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

func (s *HTTPServer) networkPolicyList(resp http.ResponseWriter, req *http.Request) (any, error) {
	args := structs.NetworkPolicyListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.NetworkPolicyListResponse
	if err := s.agent.RPC("NetworkPolicy.List", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.NetworkPolicies == nil {
		out.NetworkPolicies = make([]*structs.NetworkPolicy, 0)
	}
	return out.NetworkPolicies, nil
}

func (s *HTTPServer) networkPolicyQuery(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.NetworkPolicySpecificRequest{Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleNetworkPolicyResponse
	if err := s.agent.RPC("NetworkPolicy.GetNetworkPolicy", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.NetworkPolicy == nil {
		return nil, CodedError(http.StatusNotFound, "network policy not found")
	}
	return out.NetworkPolicy, nil
}

func (s *HTTPServer) networkPolicyUpsert(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	var policy structs.NetworkPolicy
	if err := decodeBody(req, &policy); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if name != "" && policy.Name != name {
		return nil, CodedError(http.StatusBadRequest, "Network policy name does not match request path")
	}

	args := structs.NetworkPolicyUpsertRequest{
		NetworkPolicies: []*structs.NetworkPolicy{&policy},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("NetworkPolicy.UpsertNetworkPolicies", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return nil, nil
}

func (s *HTTPServer) networkPolicyDelete(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.NetworkPolicyDeleteRequest{
		Names: []string{name},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("NetworkPolicy.DeleteNetworkPolicies", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return nil, nil
}

func (s *HTTPServer) networkPolicyStatus(resp http.ResponseWriter, req *http.Request, name string, admin bool) (any, error) {
	if name == "" {
		return nil, CodedError(http.StatusBadRequest, "missing network policy name")
	}
	args := structs.NetworkPolicyStatusRequest{Name: name, Admin: admin}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.NetworkPolicyStatusResponse
	if err := s.agent.RPC("NetworkPolicy.Status", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Allocations == nil {
		out.Allocations = make([]*structs.NetworkPolicyAllocStatus, 0)
	}
	return out.Allocations, nil
}

func (s *HTTPServer) adminNetworkPolicyList(resp http.ResponseWriter, req *http.Request) (any, error) {
	args := structs.AdminNetworkPolicyListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.AdminNetworkPolicyListResponse
	if err := s.agent.RPC("NetworkPolicy.ListAdminNetworkPolicies", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.AdminNetworkPolicies == nil {
		out.AdminNetworkPolicies = make([]*structs.AdminNetworkPolicy, 0)
	}
	return out.AdminNetworkPolicies, nil
}

func (s *HTTPServer) adminNetworkPolicyQuery(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.AdminNetworkPolicySpecificRequest{Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleAdminNetworkPolicyResponse
	if err := s.agent.RPC("NetworkPolicy.GetAdminNetworkPolicy", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.AdminNetworkPolicy == nil {
		return nil, CodedError(http.StatusNotFound, "admin network policy not found")
	}
	return out.AdminNetworkPolicy, nil
}

func (s *HTTPServer) adminNetworkPolicyUpsert(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	var policy structs.AdminNetworkPolicy
	if err := decodeBody(req, &policy); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if name != "" && policy.Name != name {
		return nil, CodedError(http.StatusBadRequest, "Admin network policy name does not match request path")
	}

	args := structs.AdminNetworkPolicyUpsertRequest{
		AdminNetworkPolicies: []*structs.AdminNetworkPolicy{&policy},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("NetworkPolicy.UpsertAdminNetworkPolicies", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return nil, nil
}

func (s *HTTPServer) adminNetworkPolicyDelete(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.AdminNetworkPolicyDeleteRequest{
		Names: []string{name},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("NetworkPolicy.DeleteAdminNetworkPolicies", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return nil, nil
}
