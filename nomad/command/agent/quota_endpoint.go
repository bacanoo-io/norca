// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// QuotasRequest handles GET /v1/quotas.
func (s *HTTPServer) QuotasRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := structs.QuotaSpecListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.QuotaSpecListResponse
	if err := s.agent.RPC("Quota.ListQuotaSpecs", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Quotas == nil {
		out.Quotas = make([]*structs.QuotaSpec, 0)
	}
	return out.Quotas, nil
}

// QuotaUsagesRequest handles GET /v1/quota-usages.
func (s *HTTPServer) QuotaUsagesRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := structs.QuotaSpecListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.QuotaUsageListResponse
	if err := s.agent.RPC("Quota.ListQuotaUsages", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Usages == nil {
		out.Usages = make([]*structs.QuotaUsage, 0)
	}
	return out.Usages, nil
}

// QuotaSpecificRequest handles /v1/quota/:name and /v1/quota/usage/:name.
func (s *HTTPServer) QuotaSpecificRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	path := strings.TrimPrefix(req.URL.Path, "/v1/quota/")
	if name, ok := strings.CutPrefix(path, "usage/"); ok {
		if req.Method != http.MethodGet {
			return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
		}
		if name == "" {
			return nil, CodedError(http.StatusBadRequest, "Missing Quota Name")
		}
		return s.quotaUsageQuery(resp, req, name)
	}

	name := path
	if name == "" {
		return nil, CodedError(http.StatusBadRequest, "Missing Quota Name")
	}
	switch req.Method {
	case http.MethodGet:
		return s.quotaSpecQuery(resp, req, name)
	case http.MethodPut, http.MethodPost:
		return s.quotaSpecUpdate(resp, req, name)
	case http.MethodDelete:
		return s.quotaSpecDelete(resp, req, name)
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// QuotaCreateRequest handles PUT/POST /v1/quota.
func (s *HTTPServer) QuotaCreateRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	return s.quotaSpecUpdate(resp, req, "")
}

func (s *HTTPServer) quotaSpecQuery(resp http.ResponseWriter, req *http.Request, name string) (interface{}, error) {
	args := structs.QuotaSpecSpecificRequest{Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleQuotaSpecResponse
	if err := s.agent.RPC("Quota.GetQuotaSpec", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Quota == nil {
		return nil, CodedError(http.StatusNotFound, "Quota not found")
	}
	return out.Quota, nil
}

func (s *HTTPServer) quotaUsageQuery(resp http.ResponseWriter, req *http.Request, name string) (interface{}, error) {
	args := structs.QuotaSpecSpecificRequest{Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleQuotaUsageResponse
	if err := s.agent.RPC("Quota.GetQuotaUsage", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Usage == nil {
		return nil, CodedError(http.StatusNotFound, "Quota usage not found")
	}
	return out.Usage, nil
}

func (s *HTTPServer) quotaSpecUpdate(resp http.ResponseWriter, req *http.Request, name string) (interface{}, error) {
	var spec structs.QuotaSpec
	if err := decodeBody(req, &spec); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	if name != "" && spec.Name != name {
		return nil, CodedError(http.StatusBadRequest, "Quota name does not match request path")
	}

	args := structs.QuotaSpecUpsertRequest{
		Quotas: []*structs.QuotaSpec{&spec},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("Quota.UpsertQuotaSpecs", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return nil, nil
}

func (s *HTTPServer) quotaSpecDelete(resp http.ResponseWriter, req *http.Request, name string) (interface{}, error) {
	args := structs.QuotaSpecDeleteRequest{
		Names: []string{name},
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("Quota.DeleteQuotaSpecs", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return nil, nil
}
