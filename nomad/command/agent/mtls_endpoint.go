// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

// apiServiceMTLSToStructs converts the mtls block of a service.
func apiServiceMTLSToStructs(in *api.ServiceMTLS) *structs.ServiceMTLS {
	if in == nil {
		return nil
	}
	out := &structs.ServiceMTLS{
		Mode:         in.Mode,
		ChangeMode:   in.ChangeMode,
		ChangeSignal: in.ChangeSignal,
		ChangeOn:     in.ChangeOn,
	}
	for _, u := range in.Upstreams {
		if u == nil {
			continue
		}
		out.Upstreams = append(out.Upstreams, &structs.ServiceMTLSUpstream{
			Name:      u.Name,
			Service:   u.Service,
			Namespace: u.Namespace,
			Region:    u.Region,
			Gateway:   u.Gateway,
			LocalPort: u.LocalPort,
		})
	}
	return out
}

// MTLSRootsRequest handles /v1/mtls/roots: the trust bundle of the region.
func (s *HTTPServer) MTLSRootsRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.MTLSRootsRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.MTLSRootsResponse
	if err := s.agent.RPC("MTLS.Roots", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Roots == nil {
		out.Roots = make([]*structs.MTLSRoot, 0)
	}
	return &api.MTLSRoots{
		TrustDomain:  out.TrustDomain,
		ActiveRootID: out.ActiveRootID,
		Roots:        mtlsRootsToAPI(out.Roots),
	}, nil
}

func mtlsRootsToAPI(roots []*structs.MTLSRoot) []*api.MTLSRoot {
	out := make([]*api.MTLSRoot, 0, len(roots))
	for _, r := range roots {
		out = append(out, &api.MTLSRoot{
			ID:           r.ID,
			Region:       r.Region,
			TrustDomain:  r.TrustDomain,
			RootCert:     r.RootCert,
			Active:       r.Active,
			NotBefore:    r.NotBefore,
			NotAfter:     r.NotAfter,
			RotatedOutAt: r.RotatedOutAt,
			CreateIndex:  r.CreateIndex,
			ModifyIndex:  r.ModifyIndex,
		})
	}
	return out
}

// MTLSRotateRequest handles /v1/mtls/ca/rotate.
func (s *HTTPServer) MTLSRotateRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.MTLSRotateRequest{}
	s.parseWriteRequest(req, &args.WriteRequest)
	var out structs.MTLSRotateResponse
	if err := s.agent.RPC("MTLS.RotateCA", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return map[string]string{"RootID": out.RootID}, nil
}

// ServiceIntentionsRequest handles /v1/service-intentions.
func (s *HTTPServer) ServiceIntentionsRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	switch req.Method {
	case http.MethodGet:
	case http.MethodPut, http.MethodPost:
		return s.serviceIntentionUpsert(resp, req)
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.ServiceIntentionListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.ServiceIntentionListResponse
	if err := s.agent.RPC("ServiceIntention.List", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Intentions == nil {
		out.Intentions = make([]*structs.ServiceIntention, 0)
	}
	return out.Intentions, nil
}

// ServiceIntentionRequest handles /v1/service-intention, with the key of the
// intention in the source, source_namespace and destination parameters.
func (s *HTTPServer) ServiceIntentionRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	q := req.URL.Query()
	source, destination, sourceNamespace := q.Get("source"), q.Get("destination"), q.Get("source_namespace")

	switch req.Method {
	case http.MethodGet:
		if source == "" || destination == "" {
			return nil, CodedError(http.StatusBadRequest, "source and destination are required")
		}
		args := structs.ServiceIntentionGetRequest{
			Source:          source,
			Destination:     destination,
			SourceNamespace: sourceNamespace,
		}
		if s.parse(resp, req, &args.Region, &args.QueryOptions) {
			return nil, nil
		}
		var out structs.SingleServiceIntentionResponse
		if err := s.agent.RPC("ServiceIntention.Get", &args, &out); err != nil {
			return nil, err
		}
		setMeta(resp, &out.QueryMeta)
		if out.Intention == nil {
			return nil, CodedError(http.StatusNotFound, "service intention not found")
		}
		return out.Intention, nil

	case http.MethodPut, http.MethodPost:
		return s.serviceIntentionUpsert(resp, req)

	case http.MethodDelete:
		if source == "" || destination == "" {
			return nil, CodedError(http.StatusBadRequest, "source and destination are required")
		}
		args := structs.ServiceIntentionDeleteRequest{
			Intentions: []*structs.ServiceIntention{{
				Source:          source,
				Destination:     destination,
				SourceNamespace: sourceNamespace,
			}},
		}
		s.parseWriteRequest(req, &args.WriteRequest)
		var out structs.GenericResponse
		if err := s.agent.RPC("ServiceIntention.Delete", &args, &out); err != nil {
			return nil, err
		}
		setIndex(resp, out.Index)
		return nil, nil

	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

func (s *HTTPServer) serviceIntentionUpsert(resp http.ResponseWriter, req *http.Request) (any, error) {
	var intention structs.ServiceIntention
	if err := decodeBody(req, &intention); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	args := structs.ServiceIntentionUpsertRequest{
		Intentions: []*structs.ServiceIntention{&intention},
	}
	s.parseWriteRequest(req, &args.WriteRequest)
	var out structs.GenericResponse
	if err := s.agent.RPC("ServiceIntention.Upsert", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return nil, nil
}
