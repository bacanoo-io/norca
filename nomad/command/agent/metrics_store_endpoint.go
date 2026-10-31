// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// MetricsHistoryRequest returns the history of a metric of a task group
// from the metrics store.
//
//	GET /v1/metrics/history?job=<id>&group=<name>&metric=norca.group.cpu&from=<time>&to=<time>&max_points=360
func (s *HTTPServer) MetricsHistoryRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(405, ErrInvalidMethod)
	}
	q := req.URL.Query()
	args := structs.MetricsHistoryRequest{
		JobID:    q.Get("job"),
		Group:    q.Get("group"),
		Metric:   q.Get("metric"),
		ServerID: q.Get("server_id"),
	}
	if done := s.parse(resp, req, &args.Region, &args.QueryOptions); done {
		return nil, nil
	}
	var err error
	if args.From, err = parseMetricsTime(q.Get("from")); err != nil {
		return nil, CodedError(400, "invalid from: "+err.Error())
	}
	if args.To, err = parseMetricsTime(q.Get("to")); err != nil {
		return nil, CodedError(400, "invalid to: "+err.Error())
	}
	if v := q.Get("max_points"); v != "" {
		if args.MaxPoints, err = strconv.Atoi(v); err != nil || args.MaxPoints < 0 {
			return nil, CodedError(400, "invalid max_points: "+v)
		}
	}

	var reply structs.MetricsHistoryResponse
	if err := s.agent.RPC(structs.MetricsStoreHistoryRPCMethod, &args, &reply); err != nil {
		return nil, err
	}
	setMeta(resp, &reply.QueryMeta)
	return reply, nil
}

// parseMetricsTime parses an RFC 3339 time, Unix seconds, or a duration
// before now ("1h"); empty is the zero time.
func parseMetricsTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(sec, 0), nil
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time, Unix seconds or a duration", v)
}

// OperatorMetricsStoreStatus returns the state of the metrics store of the
// server that answers the request, or of the server named by ?server_id=.
//
//	GET /v1/operator/metrics-store
func (s *HTTPServer) OperatorMetricsStoreStatus(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(405, ErrInvalidMethod)
	}
	args := structs.MetricsStoreStatusRequest{ServerID: req.URL.Query().Get("server_id")}
	if done := s.parse(resp, req, &args.Region, &args.QueryOptions); done {
		return nil, nil
	}
	var reply structs.MetricsStoreStatusResponse
	if err := s.agent.RPC(structs.MetricsStoreStatusRPCMethod, &args, &reply); err != nil {
		return nil, err
	}
	return reply.Status, nil
}
