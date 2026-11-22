// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca"
)

// RiggerRegistryRequest handles /v1/rigger/registry/<name>/sync (PUT or
// POST: sync the registry now), /v1/rigger/registry/<name>/search (GET: a
// page of its index at the commit of its last good sync; query kind, text,
// offset, limit), and /v1/rigger/registry/<name>/module and .../icon (GET: a
// version of a module of the registry, what an install form needs of it and
// its icon; query name, version). The registries themselves are objects of
// the kind rigger.norca.io/Registry (/v1/object/rigger.norca.io/Registry/...).
func (s *HTTPServer) RiggerRegistryRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	name, action, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/v1/rigger/registry/"), "/")
	if !ok || name == "" {
		return nil, CodedError(http.StatusNotFound, "unknown Rigger registry path")
	}

	switch action {
	case "sync":
		return s.riggerRegistrySync(resp, req, name)
	case "search":
		return s.riggerRegistrySearch(resp, req, name)
	case "module", "icon":
		return s.riggerModuleDetail(resp, req, name, action == "icon")
	default:
		return nil, CodedError(http.StatusNotFound, "unknown Rigger registry path")
	}
}

// RiggerInstallRequest handles /v1/rigger/install (PUT or POST): one release
// of a module of a synced registry, or of a local module sent as files,
// planned (query plan=true) or applied.
func (s *HTTPServer) RiggerInstallRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	var args norca.RiggerInstallRequest
	if err := decodeBody(req, &args); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	if v := req.URL.Query().Get("plan"); v != "" {
		plan, err := strconv.ParseBool(v)
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, "invalid plan: "+v)
		}

		args.Plan = plan
	}

	s.parseWriteRequest(req, &args.WriteRequest)

	var out norca.RiggerInstallResponse
	if err := s.agent.RPC("Rigger.Install", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)

	return &out, nil
}

// riggerModuleDetail answers .../module (JSON) and .../icon (the PNG).
func (s *HTTPServer) riggerModuleDetail(resp http.ResponseWriter, req *http.Request, registry string, icon bool) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := norca.RiggerDetailRequest{RiggerModuleRequest: norca.RiggerModuleRequest{
		Registry: registry, Module: req.URL.Query().Get("name"), Version: req.URL.Query().Get("version"),
	}}
	if args.Module == "" {
		return nil, CodedError(http.StatusBadRequest, "missing the name of the module (query name)")
	}

	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	if icon {
		var out norca.RiggerIconResponse
		if err := s.agent.RPC("Rigger.Icon", &args, &out); err != nil {
			return nil, err
		}

		setMeta(resp, &out.QueryMeta)
		resp.Header().Set("Content-Type", "image/png")
		_, err := resp.Write(out.Icon)

		return nil, err
	}

	var out norca.RiggerDetailResponse
	if err := s.agent.RPC("Rigger.Detail", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)

	return &out, nil
}

func (s *HTTPServer) riggerRegistrySync(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := norca.RiggerRegistryNameRequest{Name: name}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out norca.RiggerRegistryResponse
	if err := s.agent.RPC("Rigger.SyncRegistry", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)

	return out.Registry, nil
}

func (s *HTTPServer) riggerRegistrySearch(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	query := req.URL.Query()
	args := norca.RiggerRegistrySearchRequest{Name: name, Kind: query.Get("kind"), Text: query.Get("text")}

	for _, p := range []struct {
		name string
		to   *int
	}{{"offset", &args.Offset}, {"limit", &args.Limit}} {
		if v := query.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, CodedError(http.StatusBadRequest, "invalid "+p.name+": "+v)
			}

			*p.to = n
		}
	}

	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out norca.RiggerRegistrySearchResponse
	if err := s.agent.RPC("Rigger.SearchRegistry", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)

	return &out, nil
}
