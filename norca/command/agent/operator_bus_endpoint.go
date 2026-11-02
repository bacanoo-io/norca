// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strconv"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// OperatorBusStatus returns the status of the system bus of the server that
// answers the request, or of the server named by ?server_id=.
//
//	GET /v1/operator/bus
func (s *HTTPServer) OperatorBusStatus(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(405, ErrInvalidMethod)
	}

	args := structs.BusStatusRequest{ServerID: req.URL.Query().Get("server_id")}
	if done := s.parse(resp, req, &args.Region, &args.QueryOptions); done {
		return nil, nil
	}

	var reply structs.BusStatusResponse
	if err := s.agent.RPC("Operator.BusStatus", &args, &reply); err != nil {
		return nil, err
	}
	return reply.Status, nil
}

// OperatorConsulKVRestore replaces the Consul KV bucket of the system bus
// with the keys of a backup.
//
//	PUT /v1/operator/consul-kv/restore
func (s *HTTPServer) OperatorConsulKVRestore(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return nil, CodedError(405, ErrInvalidMethod)
	}

	var args structs.ConsulKVRestoreRequest
	if err := decodeBody(req, &args); err != nil {
		return nil, CodedError(400, err.Error())
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var reply structs.ConsulKVRestoreResponse
	if err := s.agent.RPC("Operator.ConsulKVRestore", &args, &reply); err != nil {
		return nil, err
	}
	setIndex(resp, reply.Index)
	return &reply, nil
}

// OperatorConsulKVMigrate copies the keys of the Consul KV kept in Variables
// into the bucket of the system bus.
//
//	POST /v1/operator/consul-kv/migrate?path=consul/kv&dry_run=true
func (s *HTTPServer) OperatorConsulKVMigrate(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return nil, CodedError(405, ErrInvalidMethod)
	}

	args := structs.ConsulKVMigrateRequest{Path: req.URL.Query().Get("path")}
	if raw := req.URL.Query().Get("dry_run"); raw != "" {
		dryRun, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, CodedError(400, "invalid dry_run value")
		}
		args.DryRun = dryRun
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var reply structs.ConsulKVMigrateResponse
	if err := s.agent.RPC("Operator.ConsulKVMigrate", &args, &reply); err != nil {
		return nil, err
	}
	setIndex(resp, reply.Index)
	return reply.Result, nil
}
