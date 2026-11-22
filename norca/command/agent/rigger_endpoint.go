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
// POST: sync the registry now) and /v1/rigger/registry/<name>/search (GET:
// a page of its index at the commit of its last good sync; query kind,
// text, offset, limit). The registries themselves are objects of the kind
// rigger.norca.io/Registry (/v1/object/rigger.norca.io/Registry/...).
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
	default:
		return nil, CodedError(http.StatusNotFound, "unknown Rigger registry path")
	}
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
