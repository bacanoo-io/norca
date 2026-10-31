// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// The HTTP API of the dynamic secrets engines and their leases (Norca,
// phase 7 of the secret store). Engines and leases never contain a
// credential: credentials are created for tasks only, over the client's
// server RPC.

// SecretEnginesListRequest serves GET /v1/secret-engines?prefix=.
func (s *HTTPServer) SecretEnginesListRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.SecretEngineListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.SecretEngineListResponse
	if err := s.agent.RPC(structs.SecretEngineListRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Engines == nil {
		out.Engines = make([]*structs.SecretEngineStub, 0)
	}
	return out.Engines, nil
}

// SecretEngineSpecificRequest serves /v1/secret-engine/:name:
//
//	GET    /v1/secret-engine/:name               the engine
//	PUT    /v1/secret-engine/:name[?cas=n]       create or update
//	DELETE /v1/secret-engine/:name[?cas=n]       delete (no live leases)
//	POST   /v1/secret-engine/:name/rotate-root   rotate the admin key
//	POST   /v1/secret-engine/:name/revoke        revoke all leases
func (s *HTTPServer) SecretEngineSpecificRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	name := strings.TrimPrefix(req.URL.Path, "/v1/secret-engine/")
	if name == "" {
		return nil, CodedError(http.StatusBadRequest, "missing secret engine name")
	}
	switch req.Method {
	case http.MethodGet:
		return s.secretEngineRead(resp, req, name)
	case http.MethodPut:
		return s.secretEngineWrite(resp, req, name)
	case http.MethodDelete:
		return s.secretEngineDelete(resp, req, name)
	case http.MethodPost:
		switch {
		case strings.HasSuffix(name, "/rotate-root"):
			return s.secretEngineRotateRoot(resp, req, strings.TrimSuffix(name, "/rotate-root"))
		case strings.HasSuffix(name, "/revoke"):
			return s.secretLeaseRevoke(resp, req, &structs.SecretLeaseRevokeRequest{Engine: strings.TrimSuffix(name, "/revoke")})
		}
		return nil, CodedError(http.StatusBadRequest, "POST is only valid for /rotate-root and /revoke; write with PUT")
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

func (s *HTTPServer) secretEngineRead(resp http.ResponseWriter, req *http.Request, name string) (interface{}, error) {
	args := structs.SecretEngineReadRequest{Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.SecretEngineResponse
	if err := s.agent.RPC(structs.SecretEngineReadRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Engine == nil {
		return nil, CodedError(http.StatusNotFound, "secret engine not found")
	}
	return out.Engine, nil
}

// secretEngineWriteResult is the body of the response of an engine write.
type secretEngineWriteResult struct {
	Engine *structs.SecretEngine
	Key    string `json:",omitempty"`
}

func (s *HTTPServer) secretEngineWrite(resp http.ResponseWriter, req *http.Request, name string) (interface{}, error) {
	var engine structs.SecretEngine
	if err := decodeBody(req, &engine); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if engine.Name == "" {
		engine.Name = name
	}
	if engine.Name != name {
		return nil, CodedError(http.StatusBadRequest, "the engine name in the body does not match the path")
	}
	args := structs.SecretEngineUpsertRequest{Engine: &engine}
	s.parseWriteRequest(req, &args.WriteRequest)
	if isCAS, index, err := parseCAS(req); err != nil {
		return nil, err
	} else if isCAS {
		args.CheckIndex = &index
	}
	var out structs.SecretEngineWriteResponse
	if err := s.agent.RPC(structs.SecretEngineUpsertRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &secretEngineWriteResult{Engine: out.Engine}, nil
}

func (s *HTTPServer) secretEngineDelete(resp http.ResponseWriter, req *http.Request, name string) (interface{}, error) {
	args := structs.SecretEngineDeleteRequest{Name: name}
	s.parseWriteRequest(req, &args.WriteRequest)
	if isCAS, index, err := parseCAS(req); err != nil {
		return nil, err
	} else if isCAS {
		args.CheckIndex = &index
	}
	var out structs.SecretEngineWriteResponse
	if err := s.agent.RPC(structs.SecretEngineDeleteRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return nil, nil
}

func (s *HTTPServer) secretEngineRotateRoot(resp http.ResponseWriter, req *http.Request, name string) (interface{}, error) {
	args := structs.SecretEngineRotateRootRequest{Name: name}
	s.parseWriteRequest(req, &args.WriteRequest)
	var out structs.SecretEngineWriteResponse
	if err := s.agent.RPC(structs.SecretEngineRotateRootRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &secretEngineWriteResult{Engine: out.Engine, Key: out.Key}, nil
}

// SecretLeasesListRequest serves GET /v1/secret-leases?engine=&prefix=.
func (s *HTTPServer) SecretLeasesListRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.SecretLeaseListRequest{Engine: req.URL.Query().Get("engine")}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.SecretLeaseListResponse
	if err := s.agent.RPC(structs.SecretLeaseListRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Leases == nil {
		out.Leases = make([]*structs.SecretLease, 0)
	}
	return out.Leases, nil
}

// SecretLeaseSpecificRequest serves /v1/secret-lease/:id:
//
//	GET  /v1/secret-lease/:id          the lease
//	POST /v1/secret-lease/:id/revoke   revoke it
func (s *HTTPServer) SecretLeaseSpecificRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	id := strings.TrimPrefix(req.URL.Path, "/v1/secret-lease/")
	if id == "" {
		return nil, CodedError(http.StatusBadRequest, "missing lease ID")
	}
	switch req.Method {
	case http.MethodGet:
		args := structs.SecretLeaseReadRequest{LeaseID: id}
		if s.parse(resp, req, &args.Region, &args.QueryOptions) {
			return nil, nil
		}
		var out structs.SecretLeaseResponse
		if err := s.agent.RPC(structs.SecretLeaseReadRPCMethod, &args, &out); err != nil {
			return nil, err
		}
		setMeta(resp, &out.QueryMeta)
		if out.Lease == nil {
			return nil, CodedError(http.StatusNotFound, "secret lease not found")
		}
		return out.Lease, nil
	case http.MethodPost:
		if !strings.HasSuffix(id, "/revoke") {
			return nil, CodedError(http.StatusBadRequest, "POST is only valid for /revoke")
		}
		return s.secretLeaseRevoke(resp, req, &structs.SecretLeaseRevokeRequest{LeaseID: strings.TrimSuffix(id, "/revoke")})
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// secretLeaseRevokeResult is the body of the response of a revocation.
type secretLeaseRevokeResult struct {
	Revoked []string
}

func (s *HTTPServer) secretLeaseRevoke(resp http.ResponseWriter, req *http.Request, args *structs.SecretLeaseRevokeRequest) (interface{}, error) {
	s.parseWriteRequest(req, &args.WriteRequest)
	var out structs.SecretLeaseRevokeResponse
	if err := s.agent.RPC(structs.SecretLeaseRevokeRPCMethod, args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	if out.Revoked == nil {
		out.Revoked = []string{}
	}
	return &secretLeaseRevokeResult{Revoked: out.Revoked}, nil
}
