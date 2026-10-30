// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/nomad"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/pack/catalog"
)

// The cluster pack registries and their catalog (nomad/nomad/
// pack_registry_endpoint.go). Writes, pack details and server-side renders
// run on the leader, which keeps the pack cache.

// PackRegistriesRequest handles GET /v1/pack-registries.
func (s *HTTPServer) PackRegistriesRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := nomad.PackRegistryListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out nomad.PackRegistryListResponse
	if err := s.agent.RPC("Packs.List", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Registries == nil {
		out.Registries = make([]*nomad.PackRegistry, 0)
	}
	return out.Registries, nil
}

// PackRegistryUpsertRequest handles POST/PUT /v1/pack-registry.
func (s *HTTPServer) PackRegistryUpsertRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	var body nomad.PackRegistry
	if err := decodeBody(req, &body); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	return s.upsertPackRegistry(resp, req, &body)
}

func (s *HTTPServer) upsertPackRegistry(resp http.ResponseWriter, req *http.Request, body *nomad.PackRegistry) (any, error) {
	args := nomad.PackRegistryUpsertRequest{Registry: body}
	s.parseWriteRequest(req, &args.WriteRequest)
	var out nomad.PackRegistryResponse
	if err := s.agent.RPC("Packs.Upsert", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out.Registry, nil
}

// PackRegistrySpecificRequest handles /v1/pack-registry/:name and /v1/pack-registry/:name/sync.
func (s *HTTPServer) PackRegistrySpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/pack-registry/"), "/")
	name := parts[0]
	if name == "" {
		return nil, CodedError(http.StatusBadRequest, "missing pack registry name")
	}

	switch {
	case len(parts) == 1:
		switch req.Method {
		case http.MethodGet:
			args := nomad.PackRegistryListRequest{}
			if s.parse(resp, req, &args.Region, &args.QueryOptions) {
				return nil, nil
			}
			var out nomad.PackRegistryListResponse
			if err := s.agent.RPC("Packs.List", &args, &out); err != nil {
				return nil, err
			}
			setMeta(resp, &out.QueryMeta)
			for _, reg := range out.Registries {
				if reg.Name == name {
					return reg, nil
				}
			}
			return nil, CodedError(http.StatusNotFound, "pack registry not found")
		case http.MethodPut, http.MethodPost:
			var body nomad.PackRegistry
			if err := decodeBody(req, &body); err != nil {
				return nil, CodedError(http.StatusBadRequest, err.Error())
			}
			body.Name = name
			return s.upsertPackRegistry(resp, req, &body)
		case http.MethodDelete:
			args := nomad.PackRegistryNameRequest{Name: name}
			s.parseWriteRequest(req, &args.WriteRequest)
			var out nomad.PackRegistryResponse
			if err := s.agent.RPC("Packs.Delete", &args, &out); err != nil {
				return nil, err
			}
			setIndex(resp, out.Index)
			return map[string]any{"Index": out.Index}, nil
		default:
			return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
		}
	case len(parts) == 2 && parts[1] == "sync":
		if req.Method != http.MethodPost && req.Method != http.MethodPut {
			return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
		}
		args := nomad.PackRegistryNameRequest{Name: name}
		s.parseWriteRequest(req, &args.WriteRequest)
		var out nomad.PackRegistryResponse
		if err := s.agent.RPC("Packs.Sync", &args, &out); err != nil {
			return nil, err
		}
		setIndex(resp, out.Index)
		return out.Registry, nil
	default:
		return nil, CodedError(http.StatusNotFound, "unknown pack registry path")
	}
}

// PacksCatalogRequest handles GET /v1/packs.
func (s *HTTPServer) PacksCatalogRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := nomad.PackRegistryListRequest{Registry: req.URL.Query().Get("registry")}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out nomad.PackRegistryListResponse
	if err := s.agent.RPC("Packs.Catalog", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Packs == nil {
		out.Packs = make([]*nomad.PackCatalogEntry, 0)
	}
	return out.Packs, nil
}

// PackDetailRequest handles GET /v1/pack/:registry/:pack.
func (s *HTTPServer) PackDetailRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/pack/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return nil, CodedError(http.StatusBadRequest, "expected /v1/pack/:registry/:pack")
	}
	args := nomad.PackDetailRequest{
		Registry: parts[0],
		Pack:     parts[1],
		Version:  req.URL.Query().Get("version"),
	}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out nomad.PackDetailRPCResponse
	if err := s.agent.RPC("Packs.Detail", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	return out.Detail, nil
}

// PackUploadRequest handles PUT/POST /v1/pack-upload?registry=&filename=,
// with the archive as the body, and DELETE /v1/pack-upload/:registry/:digest.
func (s *HTTPServer) PackUploadRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	switch req.Method {
	case http.MethodPut, http.MethodPost:
		if req.URL.Path != "/v1/pack-upload" {
			return nil, CodedError(http.StatusNotFound, "unknown pack upload path")
		}
		data, err := io.ReadAll(io.LimitReader(req.Body, catalog.MaxUploadSize+1))
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}
		if len(data) > catalog.MaxUploadSize {
			return nil, CodedError(http.StatusRequestEntityTooLarge, fmt.Sprintf("the upload is larger than %d MiB", catalog.MaxUploadSize>>20))
		}
		args := nomad.PackUploadRequest{
			Registry: req.URL.Query().Get("registry"),
			FileName: req.URL.Query().Get("filename"),
			Data:     data,
		}
		s.parseWriteRequest(req, &args.WriteRequest)
		var out nomad.PackUploadResponse
		if err := s.agent.RPC("Packs.Upload", &args, &out); err != nil {
			return nil, err
		}
		setIndex(resp, out.Index)
		return out, nil
	case http.MethodDelete:
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/pack-upload/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, CodedError(http.StatusBadRequest, "expected /v1/pack-upload/:registry/:digest")
		}
		args := nomad.PackUploadDeleteRequest{Registry: parts[0], Digest: parts[1]}
		s.parseWriteRequest(req, &args.WriteRequest)
		var out nomad.PackUploadResponse
		if err := s.agent.RPC("Packs.DeleteUpload", &args, &out); err != nil {
			return nil, err
		}
		setIndex(resp, out.Index)
		return out, nil
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// releasePackApply handles POST /v1/release/:name/plan-pack and
// /v1/release/:name/deploy-pack: the leader renders the pack of a cluster
// registry and plans or applies it.
func (s *HTTPServer) releasePackApply(resp http.ResponseWriter, req *http.Request, name string, dryRun bool) (any, error) {
	var deployReq nomad.PackDeployRequest
	if err := decodeBody(req, &deployReq); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	args := nomad.PackDeployRPCRequest{Name: name, Deploy: &deployReq, DryRun: dryRun}
	s.parseWriteRequest(req, &args.WriteRequest)
	var out structs.ReleaseApplyResponse
	if err := s.agent.RPC("Packs.Deploy", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out, nil
}
