// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// SecretsListRequest serves GET /v1/secrets?prefix=: the metadata of the
// secrets, never values.
func (s *HTTPServer) SecretsListRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := structs.SecretListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SecretListResponse
	if err := s.agent.RPC(structs.SecretListRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Secrets == nil {
		out.Secrets = make([]*structs.SecretStub, 0)
	}
	return out.Secrets, nil
}

// SecretSpecificRequest serves /v1/secret/:path:
//
//	GET    /v1/secret/:path?metadata          metadata
//	GET    /v1/secret/:path[?version=n]       value
//	PUT    /v1/secret/:path[?cas=n]           write
//	DELETE /v1/secret/:path[?versions=..]     soft delete
//	POST   /v1/secret/:path/undelete          undelete
//	POST   /v1/secret/:path/destroy           destroy
func (s *HTTPServer) SecretSpecificRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	path := strings.TrimPrefix(req.URL.Path, "/v1/secret/")
	if path == "" {
		return nil, CodedError(http.StatusBadRequest, "missing secret path")
	}
	switch req.Method {
	case http.MethodGet:
		if _, ok := req.URL.Query()["metadata"]; ok {
			return s.secretMetadata(resp, req, path)
		}
		return s.secretRead(resp, req, path)
	case http.MethodPut:
		return s.secretWrite(resp, req, path)
	case http.MethodDelete:
		return s.secretVersions(resp, req, path, structs.SecretDeleteRPCMethod)
	case http.MethodPost:
		switch {
		case strings.HasSuffix(path, "/undelete"):
			return s.secretVersions(resp, req, strings.TrimSuffix(path, "/undelete"), structs.SecretUndeleteRPCMethod)
		case strings.HasSuffix(path, "/destroy"):
			return s.secretVersions(resp, req, strings.TrimSuffix(path, "/destroy"), structs.SecretDestroyRPCMethod)
		}
		return nil, CodedError(http.StatusBadRequest, "POST is only valid for /undelete and /destroy; write with PUT")
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

func (s *HTTPServer) secretMetadata(resp http.ResponseWriter, req *http.Request, path string) (interface{}, error) {
	args := structs.SecretReadMetadataRequest{Path: path}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.SecretMetadataResponse
	if err := s.agent.RPC(structs.SecretReadMetadataRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Secret == nil {
		return nil, CodedError(http.StatusNotFound, "secret not found")
	}
	return out.Secret, nil
}

// secretValueResponse is the body of a value read.
type secretValueResponse struct {
	Secret  *structs.Secret
	Version uint64
	Items   structs.SecretItems
}

func (s *HTTPServer) secretRead(resp http.ResponseWriter, req *http.Request, path string) (interface{}, error) {
	args := structs.SecretReadRequest{Path: path}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	if v := req.URL.Query().Get("version"); v != "" {
		version, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, fmt.Sprintf("invalid version: %v", err))
		}
		args.Version = version
	}
	var out structs.SecretReadResponse
	if err := s.agent.RPC(structs.SecretReadRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Secret == nil {
		return nil, CodedError(http.StatusNotFound, "secret not found")
	}
	return &secretValueResponse{Secret: out.Secret, Version: out.Version, Items: out.Items}, nil
}

// secretWriteBody is the body of a write: the items of the new version (none
// for a metadata-only update) and the metadata (none to keep the existing
// metadata).
type secretWriteBody struct {
	Items structs.SecretItems
	Meta  *structs.SecretMeta
}

func (s *HTTPServer) secretWrite(resp http.ResponseWriter, req *http.Request, path string) (interface{}, error) {
	var body secretWriteBody
	if err := decodeBody(req, &body); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if len(body.Items) == 0 && body.Meta == nil {
		return nil, CodedError(http.StatusBadRequest, "secret write requires Items or Meta")
	}
	args := structs.SecretUpsertRequest{Path: path, Items: body.Items, Meta: body.Meta}
	s.parseWriteRequest(req, &args.WriteRequest)
	if isCAS, index, err := parseCAS(req); err != nil {
		return nil, err
	} else if isCAS {
		args.CheckIndex = &index
	}

	var out structs.SecretWriteResponse
	if err := s.agent.RPC(structs.SecretUpsertRPCMethod, &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &secretWriteResult{Secret: out.Secret}, nil
}

// secretWriteResult is the body of the response of a write: the metadata of
// the secret afterwards, null once the whole secret was destroyed.
type secretWriteResult struct {
	Secret *structs.Secret
}

func (s *HTTPServer) secretVersions(resp http.ResponseWriter, req *http.Request, path, method string) (interface{}, error) {
	if path == "" {
		return nil, CodedError(http.StatusBadRequest, "missing secret path")
	}
	args := structs.SecretVersionsRequest{Path: path}
	s.parseWriteRequest(req, &args.WriteRequest)
	if v := req.URL.Query().Get("versions"); v != "" {
		for _, part := range strings.Split(v, ",") {
			version, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
			if err != nil || version == 0 {
				return nil, CodedError(http.StatusBadRequest, fmt.Sprintf("invalid version %q", part))
			}
			args.Versions = append(args.Versions, version)
		}
	}
	if isCAS, index, err := parseCAS(req); err != nil {
		return nil, err
	} else if isCAS {
		args.CheckIndex = &index
	}

	var out structs.SecretWriteResponse
	if err := s.agent.RPC(method, &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &secretWriteResult{Secret: out.Secret}, nil
}
