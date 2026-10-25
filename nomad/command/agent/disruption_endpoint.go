// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/hashicorp/nomad/nomad/structs"
)

// allocEvict handles POST /v1/allocation/:id/evict. If the disruption budget
// of the allocation's task group does not allow the eviction, it responds
// with 429 and a Retry-After header.
func (s *HTTPServer) allocEvict(allocID string, resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if !(req.Method == http.MethodPost || req.Method == http.MethodPut) {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	noShutdownDelay := false
	if v := req.URL.Query().Get("no_shutdown_delay"); v != "" {
		var err error
		noShutdownDelay, err = strconv.ParseBool(v)
		if err != nil {
			return nil, CodedError(http.StatusBadRequest,
				fmt.Sprintf("no_shutdown_delay value is not a boolean: %v", err))
		}
	}

	args := &structs.AllocEvictRequest{
		AllocID:         allocID,
		NoShutdownDelay: noShutdownDelay,
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.AllocEvictResponse
	if err := s.agent.RPC("Alloc.Evict", args, &out); err != nil {
		if structs.IsErrUnknownAllocation(err) {
			return nil, CodedError(http.StatusNotFound, allocNotFoundErr)
		}
		if code, _, ok := structs.CodeFromRPCCodedErr(err); ok && code == http.StatusTooManyRequests {
			resp.Header().Set("Retry-After", strconv.Itoa(structs.DisruptionRetryAfterSeconds))
		}
		return nil, err
	}

	setIndex(resp, out.Index)
	return &out, nil
}

// jobDisruptionRequest handles GET /v1/job/:id/disruption.
func (s *HTTPServer) jobDisruptionRequest(resp http.ResponseWriter, req *http.Request, jobID string) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}

	args := structs.JobDisruptionRequest{JobID: jobID}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.JobDisruptionResponse
	if err := s.agent.RPC("Job.Disruption", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if !out.JobFound {
		return nil, CodedError(http.StatusNotFound, jobNotFoundErr)
	}
	if out.TaskGroups == nil {
		out.TaskGroups = make(map[string]*structs.DisruptionStatus)
	}
	return out.TaskGroups, nil
}
