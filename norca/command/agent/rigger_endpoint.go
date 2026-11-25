// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/norca"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// RiggerRegistryRequest handles /v1/rigger/registry/<name>/sync (PUT or
// POST: sync the registry now), /v1/rigger/registry/<name>/search (GET: a
// page of its index at the commit of its last good sync; query kind, text,
// category, keyword, offset, limit), /v1/rigger/registry/<name>/module and
// .../icon (GET: a version of a module of the registry, what an install form
// needs of it and its icon; query name, version), and .../stack (GET: the
// same of a published stack). The registries themselves are objects of
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
	case "stack":
		return s.riggerStackDetail(resp, req, name)
	default:
		return nil, CodedError(http.StatusNotFound, "unknown Rigger registry path")
	}
}

// RiggerInstallRequest handles /v1/rigger/install (PUT or POST): one release
// of a module of a synced registry, of a module built into the servers, or
// of a local module sent as files, planned (query plan=true) or applied.
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
	args := norca.RiggerRegistrySearchRequest{
		Name: name, Kind: query.Get("kind"), Text: query.Get("text"),
		Category: query.Get("category"), Keyword: query.Get("keyword"),
	}

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

// RiggerStackRequest handles /v1/rigger/stack/<action> (PUT or POST): plan
// and status answer at once, apply and destroy start an operation and answer
// with it. The body names a published stack (Source, Version) or sends a
// local one (Files); the namespace of the request is that of the stack.
func (s *HTTPServer) RiggerStackRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	var args norca.RiggerStackRequest
	if err := decodeBody(req, &args); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	s.parseWriteRequest(req, &args.WriteRequest)

	switch strings.TrimPrefix(req.URL.Path, "/v1/rigger/stack/") {
	case "plan":
		var out norca.RiggerStackPlanResponse
		if err := s.agent.RPC("Rigger.StackPlan", &args, &out); err != nil {
			return nil, err
		}

		setIndex(resp, out.Index)

		return &out, nil
	case "status":
		var out norca.RiggerStackStatusResponse
		if err := s.agent.RPC("Rigger.StackStatus", &args, &out); err != nil {
			return nil, err
		}

		setIndex(resp, out.Index)

		return &out, nil
	case "apply", "destroy":
		method := "Rigger.StackApply"
		if strings.HasSuffix(req.URL.Path, "/destroy") {
			method = "Rigger.StackDestroy"
		}

		var out norca.RiggerOperationResponse
		if err := s.agent.RPC(method, &args, &out); err != nil {
			return nil, err
		}

		setIndex(resp, out.Index)

		return out.Operation, nil
	default:
		return nil, CodedError(http.StatusNotFound, "unknown Rigger stack path")
	}
}

// RiggerOperationRequest handles /v1/rigger/operation/<name>/cancel (PUT or
// POST: stop the operation before its next release) and
// /v1/rigger/operation/<name>/log (GET: a page of its log; query after,
// limit, wait). The operations themselves are objects of the kind
// rigger.norca.io/Operation (/v1/object/rigger.norca.io/Operation/...).
func (s *HTTPServer) RiggerOperationRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	name, action, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/v1/rigger/operation/"), "/")
	if !ok || name == "" {
		return nil, CodedError(http.StatusNotFound, "unknown Rigger operation path")
	}

	switch action {
	case "cancel":
		if req.Method != http.MethodPut && req.Method != http.MethodPost {
			return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
		}

		args := norca.RiggerOperationNameRequest{Name: name}
		s.parseWriteRequest(req, &args.WriteRequest)

		var out norca.RiggerOperationResponse
		if err := s.agent.RPC("Rigger.CancelOperation", &args, &out); err != nil {
			return nil, err
		}

		setIndex(resp, out.Index)

		return out.Operation, nil
	case "log":
		return s.riggerOperationLog(resp, req, name)
	default:
		return nil, CodedError(http.StatusNotFound, "unknown Rigger operation path")
	}
}

func (s *HTTPServer) riggerOperationLog(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := norca.RiggerOperationLogRequest{Name: name}
	query := req.URL.Query()

	if v := query.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, "invalid after: "+v)
		}

		args.After = n
	}

	if v := query.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, "invalid limit: "+v)
		}

		args.Limit = n
	}

	if v := query.Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, "invalid wait: "+v)
		}

		args.Wait = d
	}

	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out norca.RiggerOperationLogResponse
	if err := s.agent.RPC("Rigger.OperationLog", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)

	return &out, nil
}

// riggerStackDetail answers .../stack: a version of a published stack of
// the registry (query name, version).
func (s *HTTPServer) riggerStackDetail(resp http.ResponseWriter, req *http.Request, registry string) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := norca.RiggerDetailRequest{RiggerModuleRequest: norca.RiggerModuleRequest{
		Registry: registry, Module: req.URL.Query().Get("name"), Version: req.URL.Query().Get("version"),
	}}
	if args.Module == "" {
		return nil, CodedError(http.StatusBadRequest, "missing the name of the stack (query name)")
	}

	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out norca.RiggerStackDetailResponse
	if err := s.agent.RPC("Rigger.StackDetail", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)

	return &out, nil
}

// riggerBundleMaxBody bounds the body of a bundle import; the servers bound
// the bundle again.
const riggerBundleMaxBody = 64<<20 + 1

// RiggerBundlesRequest handles GET /v1/rigger/bundles: the bundles kept in
// the cluster.
func (s *HTTPServer) RiggerBundlesRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	var args norca.RiggerBundleListRequest
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out norca.RiggerBundleListResponse
	if err := s.agent.RPC("Rigger.ListBundles", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)

	return out.Bundles, nil
}

// RiggerBundleRequest handles PUT or POST /v1/rigger/bundle (the archive of
// rigger bundle create as the body) and DELETE /v1/rigger/bundle/<name>.
func (s *HTTPServer) RiggerBundleRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	name := strings.TrimPrefix(strings.TrimPrefix(req.URL.Path, "/v1/rigger/bundle"), "/")

	switch {
	case name == "" && (req.Method == http.MethodPut || req.Method == http.MethodPost):
		data, err := io.ReadAll(io.LimitReader(req.Body, riggerBundleMaxBody))
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}

		if len(data) >= riggerBundleMaxBody {
			return nil, CodedError(http.StatusRequestEntityTooLarge, "the bundle is larger than 64MiB")
		}

		args := norca.RiggerBundleImportRequest{Bundle: data}
		s.parseWriteRequest(req, &args.WriteRequest)

		var out norca.RiggerBundleResponse
		if err := s.agent.RPC("Rigger.ImportBundle", &args, &out); err != nil {
			return nil, err
		}

		setIndex(resp, out.Index)

		return out.Bundle, nil
	case name != "" && req.Method == http.MethodDelete:
		args := norca.RiggerBundleNameRequest{Name: name}
		s.parseWriteRequest(req, &args.WriteRequest)

		var out structs.GenericResponse
		if err := s.agent.RPC("Rigger.DeleteBundle", &args, &out); err != nil {
			return nil, err
		}

		setIndex(resp, out.Index)

		return nil, nil
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}
