// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"

	"github.com/hashicorp/nomad/nomad/structs"
)

// objectSubresourceRequest handles /v1/object/:group/:kind/:name/:sub.
func (s *HTTPServer) objectSubresourceRequest(resp http.ResponseWriter, req *http.Request, group, kind, name, sub string) (any, error) {
	isWrite := req.Method == http.MethodPut || req.Method == http.MethodPost
	switch {
	case sub == "status" && isWrite:
		return s.objectStatusUpdate(resp, req, group, kind, name)
	case sub == "finalizers" && isWrite:
		return s.objectFinalizersUpdate(resp, req, group, kind, name)
	case sub == "events" && req.Method == http.MethodGet:
		return s.objectEventsQuery(resp, req, group, kind, name)
	case sub == "events" && isWrite:
		return s.objectEventsRecord(resp, req, group, kind, name)
	case sub == "scale" && req.Method == http.MethodGet:
		return s.objectScaleQuery(resp, req, group, kind, name)
	case sub == "scale" && isWrite:
		return s.objectScaleUpdate(resp, req, group, kind, name)
	case sub == "status" || sub == "finalizers" || sub == "events" || sub == "scale":
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	return nil, CodedError(http.StatusNotFound, "unknown object subresource "+sub)
}

func (s *HTTPServer) objectFinalizersUpdate(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	var body struct {
		Add    []string
		Remove []string
	}
	if err := decodeBody(req, &body); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	cas, dryRun, err := parseObjectWriteOptions(req)
	if err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	args := structs.CustomObjectFinalizersRequest{
		Group:  group,
		Kind:   kind,
		Name:   name,
		Add:    body.Add,
		Remove: body.Remove,
		CAS:    cas,
		DryRun: dryRun,
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.CustomObjectWriteResponse
	if err := s.agent.RPC("CustomObject.UpdateFinalizers", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out.Object, nil
}

func (s *HTTPServer) objectEventsQuery(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	args := structs.CustomObjectSpecificRequest{Group: group, Kind: kind, Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.CustomObjectEventsResponse
	if err := s.agent.RPC("CustomObject.ListEvents", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Events == nil {
		out.Events = make([]*structs.ObjectEvent, 0)
	}
	return out.Events, nil
}

func (s *HTTPServer) objectEventsRecord(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	var body struct {
		Events []*structs.ObjectEvent
	}
	if err := decodeBody(req, &body); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	args := structs.CustomObjectEventsRequest{Group: group, Kind: kind, Name: name, Events: body.Events}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("CustomObject.RecordEvents", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return nil, nil
}

func (s *HTTPServer) objectScaleQuery(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	args := structs.CustomObjectSpecificRequest{Group: group, Kind: kind, Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleObjectScaleResponse
	if err := s.agent.RPC("CustomObject.GetScale", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	return out.Scale, nil
}

func (s *HTTPServer) objectScaleUpdate(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	var body struct {
		Replicas *int64
		Message  string
		Meta     map[string]string
	}
	if err := decodeBody(req, &body); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if body.Replicas == nil {
		return nil, CodedError(http.StatusBadRequest, "missing Replicas")
	}
	cas, dryRun, err := parseObjectWriteOptions(req)
	if err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	args := structs.CustomObjectScaleRequest{
		Group:    group,
		Kind:     kind,
		Name:     name,
		Replicas: *body.Replicas,
		Message:  body.Message,
		Meta:     body.Meta,
		CAS:      cas,
		DryRun:   dryRun,
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.CustomObjectScaleResponse
	if err := s.agent.RPC("CustomObject.Scale", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out.Scale, nil
}
