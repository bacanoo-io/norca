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

// ObjectKindsRequest handles /v1/object-kinds.
func (s *HTTPServer) ObjectKindsRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := structs.ObjectKindListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.ObjectKindListResponse
	if err := s.agent.RPC("ObjectKind.List", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.ObjectKinds == nil {
		out.ObjectKinds = make([]*structs.ObjectKind, 0)
	}
	return out.ObjectKinds, nil
}

// ObjectKindSpecificRequest handles /v1/object-kind/:group/:kind.
func (s *HTTPServer) ObjectKindSpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/object-kind/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, CodedError(http.StatusBadRequest, "path must be /v1/object-kind/<group>/<kind>")
	}
	group, kind := parts[0], parts[1]

	switch req.Method {
	case http.MethodGet:
		return s.objectKindQuery(resp, req, group, kind)
	case http.MethodPut, http.MethodPost:
		return s.objectKindUpsert(resp, req, group, kind)
	case http.MethodDelete:
		return s.objectKindDelete(resp, req, group, kind)
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

func (s *HTTPServer) objectKindQuery(resp http.ResponseWriter, req *http.Request, group, kind string) (any, error) {
	args := structs.ObjectKindSpecificRequest{Group: group, Kind: kind}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleObjectKindResponse
	if err := s.agent.RPC("ObjectKind.GetObjectKind", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.ObjectKind == nil {
		return nil, CodedError(http.StatusNotFound, "object kind not found")
	}
	return out.ObjectKind, nil
}

func (s *HTTPServer) objectKindUpsert(resp http.ResponseWriter, req *http.Request, group, kind string) (any, error) {
	var objectKind structs.ObjectKind
	if err := decodeBody(req, &objectKind); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if objectKind.Group == "" && objectKind.Kind == "" {
		objectKind.Group, objectKind.Kind = group, kind
	}
	if objectKind.Group != group || objectKind.Kind != kind {
		return nil, CodedError(http.StatusBadRequest, "Object kind does not match request path")
	}
	dryRun, err := parseBool(req, "dry_run")
	if err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	args := structs.ObjectKindUpsertRequest{
		ObjectKind: &objectKind,
		DryRun:     dryRun != nil && *dryRun,
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.ObjectKindUpsertResponse
	if err := s.agent.RPC("ObjectKind.UpsertObjectKind", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return out.ObjectKind, nil
}

func (s *HTTPServer) objectKindDelete(resp http.ResponseWriter, req *http.Request, group, kind string) (any, error) {
	args := structs.ObjectKindDeleteRequest{Group: group, Kind: kind}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("ObjectKind.DeleteObjectKind", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return nil, nil
}

// ObjectsRequest handles /v1/objects/:group/:kind. Objects can be selected
// by labels with one or more "label=key=value" parameters.
func (s *HTTPServer) ObjectsRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/objects/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, CodedError(http.StatusBadRequest, "path must be /v1/objects/<group>/<kind>")
	}

	args := structs.CustomObjectListRequest{Group: parts[0], Kind: parts[1]}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	for _, selector := range req.URL.Query()["label"] {
		key, value, ok := strings.Cut(selector, "=")
		if !ok || key == "" {
			return nil, CodedError(http.StatusBadRequest, fmt.Sprintf("invalid label selector %q, must be key=value", selector))
		}
		if args.Labels == nil {
			args.Labels = make(map[string]string)
		}
		args.Labels[key] = value
	}

	var out structs.CustomObjectListResponse
	if err := s.agent.RPC("CustomObject.List", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Objects == nil {
		out.Objects = make([]*structs.CustomObject, 0)
	}
	return out.Objects, nil
}

// ObjectSpecificRequest handles /v1/object/:group/:kind/:name and its
// subresources: status, finalizers, events and scale.
func (s *HTTPServer) ObjectSpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/object/"), "/")
	if len(parts) == 4 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
		return s.objectSubresourceRequest(resp, req, parts[0], parts[1], parts[2], parts[3])
	}
	switch {
	case len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "":
		return nil, CodedError(http.StatusBadRequest, "path must be /v1/object/<group>/<kind>/<name>")
	}
	group, kind, name := parts[0], parts[1], parts[2]

	switch req.Method {
	case http.MethodGet:
		return s.objectQuery(resp, req, group, kind, name)
	case http.MethodPut, http.MethodPost:
		return s.objectApply(resp, req, group, kind, name)
	case http.MethodDelete:
		return s.objectDelete(resp, req, group, kind, name)
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// parseObjectWriteOptions parses the cas and dry_run parameters.
func parseObjectWriteOptions(req *http.Request) (*uint64, bool, error) {
	var cas *uint64
	if str := req.URL.Query().Get("cas"); str != "" {
		v, err := strconv.ParseUint(str, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("Failed to parse value of %q (%v) as an index: %v", "cas", str, err)
		}
		cas = &v
	}
	dryRun, err := parseBool(req, "dry_run")
	if err != nil {
		return nil, false, err
	}
	return cas, dryRun != nil && *dryRun, nil
}

func (s *HTTPServer) objectQuery(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	args := structs.CustomObjectSpecificRequest{Group: group, Kind: kind, Name: name}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleCustomObjectResponse
	if err := s.agent.RPC("CustomObject.GetCustomObject", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Object == nil {
		return nil, CodedError(http.StatusNotFound, "object not found")
	}
	return out.Object, nil
}

func (s *HTTPServer) objectApply(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	var obj structs.CustomObject
	if err := decodeBody(req, &obj); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	if obj.Group == "" && obj.Kind == "" && obj.Name == "" {
		obj.Group, obj.Kind, obj.Name = group, kind, name
	}
	if obj.Group != group || obj.Kind != kind || obj.Name != name {
		return nil, CodedError(http.StatusBadRequest, "Object does not match request path")
	}
	cas, dryRun, err := parseObjectWriteOptions(req)
	if err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	args := structs.CustomObjectApplyRequest{Object: &obj, CAS: cas, DryRun: dryRun}
	s.parseWriteRequest(req, &args.WriteRequest)
	if args.Namespace == "" {
		args.Namespace = obj.Namespace
	}

	var out structs.CustomObjectWriteResponse
	if err := s.agent.RPC("CustomObject.Apply", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return out.Object, nil
}

func (s *HTTPServer) objectStatusUpdate(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	var body struct {
		Status map[string]any
	}
	if err := decodeBody(req, &body); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	cas, dryRun, err := parseObjectWriteOptions(req)
	if err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}

	args := structs.CustomObjectStatusRequest{
		Group:  group,
		Kind:   kind,
		Name:   name,
		Status: body.Status,
		CAS:    cas,
		DryRun: dryRun,
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.CustomObjectWriteResponse
	if err := s.agent.RPC("CustomObject.UpdateStatus", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return out.Object, nil
}

func (s *HTTPServer) objectDelete(resp http.ResponseWriter, req *http.Request, group, kind, name string) (any, error) {
	cas, dryRun, err := parseObjectWriteOptions(req)
	if err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	args := structs.CustomObjectDeleteRequest{
		Group:       group,
		Kind:        kind,
		Name:        name,
		CAS:         cas,
		DryRun:      dryRun,
		Propagation: req.URL.Query().Get("propagation"),
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.CustomObjectWriteResponse
	if err := s.agent.RPC("CustomObject.Delete", &args, &out); err != nil {
		return nil, err
	}

	setIndex(resp, out.Index)
	return out.Object, nil
}
