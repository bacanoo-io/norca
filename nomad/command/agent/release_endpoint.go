// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

// releaseApplyBody is the body of release installs and upgrades. Jobs are
// in the API form; the other resources decode into their server types like
// the bodies of their own endpoints.
type releaseApplyBody struct {
	Pack          *structs.ReleasePack
	Description   string
	Values        map[string]any
	ValuesHash    string
	Resources     []*releaseResourceBody
	Secrets       []*structs.ReleaseVariableSecret
	Atomic        *bool
	Wait          bool
	Timeout       time.Duration
	TakeOwnership bool
	MaxHistory    int
	RecordOnly    bool

	VariablesSchemas []*structs.ReleaseVariablesSchema
	VariableValues   map[string]map[string]any
	Regions          []string
}

type releaseResourceBody struct {
	Type            string
	Source          string
	Namespace       *structs.Namespace
	Quota           *structs.QuotaSpec
	ACLPolicy       *structs.ACLPolicy
	Variable        *structs.VariableDecrypted
	ObjectKind      *structs.ObjectKind
	AdmissionPolicy *structs.AdmissionPolicy
	Object          *structs.CustomObject
	Job             *api.Job
	Submission      *api.JobSubmission
	SecretKeys      []string
}

// releaseRollbackBody is the body of release rollbacks.
type releaseRollbackBody struct {
	Revision uint64
	Atomic   *bool
	Wait     bool
	Timeout  time.Duration
	DryRun   bool
}

// ReleasesRequest handles /v1/releases.
func (s *HTTPServer) ReleasesRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.ReleaseListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.ReleaseListResponse
	if err := s.agent.RPC("Release.List", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Releases == nil {
		out.Releases = make([]*structs.Release, 0)
	}
	return out.Releases, nil
}

// ReleaseSpecificRequest handles /v1/release/:name and its sub-paths: plan,
// history, revision/:n, rollback, test and drift.
func (s *HTTPServer) ReleaseSpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/release/"), "/")
	name := parts[0]
	if name == "" {
		return nil, CodedError(http.StatusBadRequest, "missing release name")
	}

	switch {
	case len(parts) == 1:
		switch req.Method {
		case http.MethodGet:
			return s.releaseQuery(resp, req, name)
		case http.MethodPut, http.MethodPost:
			return s.releaseApply(resp, req, name, false)
		case http.MethodDelete:
			return s.releaseDelete(resp, req, name)
		}
	case len(parts) == 2 && parts[1] == "plan":
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			return s.releaseApply(resp, req, name, true)
		}
	case len(parts) == 2 && parts[1] == "plan-pack":
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			return s.releasePackApply(resp, req, name, true)
		}
	case len(parts) == 2 && parts[1] == "deploy-pack":
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			return s.releasePackApply(resp, req, name, false)
		}
	case len(parts) == 2 && parts[1] == "history":
		if req.Method == http.MethodGet {
			return s.releaseHistory(resp, req, name)
		}
	case len(parts) == 3 && parts[1] == "revision":
		if req.Method == http.MethodGet {
			n, err := strconv.ParseUint(parts[2], 10, 64)
			if err != nil || n == 0 {
				return nil, CodedError(http.StatusBadRequest, "invalid revision")
			}
			return s.releaseRevision(resp, req, name, n)
		}
	case len(parts) == 2 && parts[1] == "rollback":
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			return s.releaseRollback(resp, req, name)
		}
	case len(parts) == 2 && parts[1] == "test":
		if req.Method == http.MethodPut || req.Method == http.MethodPost {
			return s.releaseTest(resp, req, name)
		}
	case len(parts) == 2 && parts[1] == "drift":
		if req.Method == http.MethodGet || req.Method == http.MethodPut || req.Method == http.MethodPost {
			return s.releaseDrift(resp, req, name)
		}
	default:
		return nil, CodedError(http.StatusNotFound, "unknown release path")
	}
	return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
}

func (s *HTTPServer) releaseQuery(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.ReleaseSpecificRequest{Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.SingleReleaseResponse
	if err := s.agent.RPC("Release.GetRelease", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Release == nil {
		return nil, CodedError(http.StatusNotFound, "release not found")
	}
	return out.Release, nil
}

func (s *HTTPServer) releaseApply(resp http.ResponseWriter, req *http.Request, name string, dryRun bool) (any, error) {
	var body releaseApplyBody
	if err := decodeBody(req, &body); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	return s.applyReleaseBody(resp, req, name, &body, dryRun)
}

func (s *HTTPServer) applyReleaseBody(resp http.ResponseWriter, req *http.Request, name string, body *releaseApplyBody, dryRun bool) (any, error) {
	var wr structs.WriteRequest
	s.parseWriteRequest(req, &wr)
	args, err := releaseRequestFromBody(name, body, wr.Namespace)
	if err != nil {
		return nil, err
	}
	args.DryRun = dryRun
	args.WriteRequest = wr

	var out structs.ReleaseApplyResponse
	if err := s.agent.RPC("Release.Apply", args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out, nil
}

// releaseRequestFromRendered converts a release request rendered by the
// pack tooling (api types) into the request of the servers. The servers
// use it for the packs they render (nomad.Config.ReleaseFromRendered).
func releaseRequestFromRendered(name string, rendered *api.ReleaseApplyRequest, namespace string) (*structs.ReleaseApplyRequest, error) {
	raw, err := json.Marshal(rendered)
	if err != nil {
		return nil, err
	}
	var body releaseApplyBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return releaseRequestFromBody(name, &body, namespace)
}

// releaseRequestFromBody converts a release request body; jobs without a
// namespace belong to the namespace of the release.
func releaseRequestFromBody(name string, body *releaseApplyBody, namespace string) (*structs.ReleaseApplyRequest, error) {
	args := &structs.ReleaseApplyRequest{
		Name:          name,
		Pack:          body.Pack,
		Description:   body.Description,
		Values:        body.Values,
		ValuesHash:    body.ValuesHash,
		Secrets:       body.Secrets,
		Atomic:        body.Atomic,
		Wait:          body.Wait,
		Timeout:       body.Timeout,
		TakeOwnership: body.TakeOwnership,
		MaxHistory:    body.MaxHistory,
		RecordOnly:    body.RecordOnly,

		VariablesSchemas: body.VariablesSchemas,
		VariableValues:   body.VariableValues,
		Regions:          body.Regions,
	}
	if namespace == "" {
		namespace = structs.DefaultNamespace
	}

	for _, r := range body.Resources {
		if r == nil {
			return nil, CodedError(http.StatusBadRequest, "empty resource")
		}
		res := &structs.ReleaseResource{
			Type:            r.Type,
			Source:          r.Source,
			Namespace:       r.Namespace,
			Quota:           r.Quota,
			ACLPolicy:       r.ACLPolicy,
			Variable:        r.Variable,
			ObjectKind:      r.ObjectKind,
			AdmissionPolicy: r.AdmissionPolicy,
			Object:          r.Object,
			SecretKeys:      r.SecretKeys,
		}
		if r.Job != nil {
			// Jobs without a namespace belong to the namespace of the
			// release, and jobs without a region to its region.
			if r.Job.Namespace == nil || *r.Job.Namespace == "" {
				r.Job.Namespace = &namespace
			}
			res.Job = ApiJobToStructJob(r.Job)
			res.Submission = apiJobSubmissionToStructs(r.Submission)
		}
		args.Resources = append(args.Resources, res)
	}
	return args, nil
}

func (s *HTTPServer) releaseDelete(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.ReleaseDeleteRequest{
		Name:        name,
		Propagation: req.URL.Query().Get("propagation"),
	}
	noHooks, err := parseBool(req, "no_hooks")
	if err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	args.NoHooks = noHooks != nil && *noHooks
	if t := req.URL.Query().Get("timeout"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, "invalid timeout: "+err.Error())
		}
		args.Timeout = d
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.ReleaseDeleteResponse
	if err := s.agent.RPC("Release.Delete", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out, nil
}

func (s *HTTPServer) releaseHistory(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.ReleaseSpecificRequest{Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.ReleaseHistoryResponse
	if err := s.agent.RPC("Release.History", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Revisions == nil {
		out.Revisions = make([]*structs.ReleaseRevision, 0)
	}
	return out.Revisions, nil
}

func (s *HTTPServer) releaseRevision(resp http.ResponseWriter, req *http.Request, name string, revision uint64) (any, error) {
	args := structs.ReleaseRevisionRequest{Name: name, Revision: revision}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.SingleReleaseRevisionResponse
	if err := s.agent.RPC("Release.GetRevision", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Revision == nil {
		return nil, CodedError(http.StatusNotFound, "revision not found")
	}
	return out.Revision, nil
}

func (s *HTTPServer) releaseRollback(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	var body releaseRollbackBody
	if req.ContentLength != 0 {
		if err := decodeBody(req, &body); err != nil {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}
	}
	args := structs.ReleaseRollbackRequest{
		Name:     name,
		Revision: body.Revision,
		Atomic:   body.Atomic,
		Wait:     body.Wait,
		Timeout:  body.Timeout,
		DryRun:   body.DryRun,
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.ReleaseApplyResponse
	if err := s.agent.RPC("Release.Rollback", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out, nil
}

func (s *HTTPServer) releaseTest(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.ReleaseTestRequest{Name: name}
	if t := req.URL.Query().Get("timeout"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return nil, CodedError(http.StatusBadRequest, "invalid timeout: "+err.Error())
		}
		args.Timeout = d
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.ReleaseTestResponse
	if err := s.agent.RPC("Release.Test", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out.Result, nil
}

func (s *HTTPServer) releaseDrift(resp http.ResponseWriter, req *http.Request, name string) (any, error) {
	args := structs.ReleaseDriftRequest{Name: name}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.ReleaseDriftResponse
	if err := s.agent.RPC("Release.Drift", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	if out.Resources == nil {
		out.Resources = make([]*structs.ReleaseDriftEntry, 0)
	}
	return out, nil
}
