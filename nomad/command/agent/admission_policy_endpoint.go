// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// AdmissionPoliciesRequest handles /v1/admission-policies.
func (s *HTTPServer) AdmissionPoliciesRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	switch req.Method {
	case http.MethodGet:
		return s.admissionPolicyList(resp, req)
	case http.MethodPut, http.MethodPost:
		return s.admissionPolicyUpsert(resp, req, "")
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// AdmissionPolicySpecificRequest handles /v1/admission-policy/:name.
func (s *HTTPServer) AdmissionPolicySpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	name := strings.TrimPrefix(req.URL.Path, "/v1/admission-policy/")
	if name == "" {
		return nil, CodedError(http.StatusBadRequest, "missing admission policy name")
	}

	switch req.Method {
	case http.MethodGet:
		return s.admissionPolicyQuery(resp, req, name)
	case http.MethodPut, http.MethodPost:
		return s.admissionPolicyUpsert(resp, req, name)
	case http.MethodDelete:
		return s.admissionPolicyDelete(resp, req, name)
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

func (s *HTTPServer) admissionPolicyList(resp http.ResponseWriter, req *http.Request) (any, error) {
	args := structs.AdmissionPolicyListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.AdmissionPolicyListResponse
	if err := s.agent.RPC("AdmissionPolicy.List", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.AdmissionPolicies == nil {
		out.AdmissionPolicies = make([]*structs.AdmissionPolicy, 0)
	}
	return out.AdmissionPolicies, nil
}

func (s *HTTPServer) admissionPolicyQuery(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.AdmissionPolicySpecificRequest{Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleAdmissionPolicyResponse
	if err := s.agent.RPC("AdmissionPolicy.GetAdmissionPolicy", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.AdmissionPolicy == nil {
		return nil, CodedError(http.StatusNotFound, "admission policy not found")
	}
	return out.AdmissionPolicy, nil
}

func (s *HTTPServer) admissionPolicyUpsert(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	var policy structs.AdmissionPolicy
	if err := decodeBody(req, &policy); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if name != "" && policy.Name != name {
		return nil, CodedError(http.StatusBadRequest, "Admission policy name does not match request path")
	}

	args := structs.AdmissionPolicyUpsertRequest{
		AdmissionPolicies: []*structs.AdmissionPolicy{&policy},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("AdmissionPolicy.UpsertAdmissionPolicies", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return nil, nil
}

func (s *HTTPServer) admissionPolicyDelete(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.AdmissionPolicyDeleteRequest{
		Names: []string{name},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("AdmissionPolicy.DeleteAdmissionPolicies", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return nil, nil
}
