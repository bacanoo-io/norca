// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// CDCConsumerRequest is the consumer the servers keep for the reader of a
// change stream (change data capture); the reader is the token or workload
// identity of the request.
//
//	PUT    /v1/cdc/consumer/:stream   create or change it (body: CDCConsumerSpec)
//	GET    /v1/cdc/consumer/:stream   read it
//	DELETE /v1/cdc/consumer/:stream   delete it
//
// :stream is events, admin or kv.
func (s *HTTPServer) CDCConsumerRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	stream := strings.TrimPrefix(req.URL.Path, "/v1/cdc/consumer/")
	if stream == "" || strings.Contains(stream, "/") {
		return nil, CodedError(http.StatusNotFound, "a change stream is events, admin or kv")
	}
	switch req.Method {
	case http.MethodPut, http.MethodPost:
		args := structs.CDCConsumerUpsertRequest{Stream: stream}
		if err := decodeBody(req, &args.Consumer); err != nil {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}
		s.parseWriteRequest(req, &args.WriteRequest)
		var reply structs.CDCConsumerWriteResponse
		if err := s.agent.RPC(structs.CDCConsumerUpsertRPCMethod, &args, &reply); err != nil {
			return nil, err
		}
		return reply.Consumer, nil
	case http.MethodGet:
		args := structs.CDCConsumerReadRequest{Stream: stream}
		if done := s.parse(resp, req, &args.Region, &args.QueryOptions); done {
			return nil, nil
		}
		var reply structs.CDCConsumerResponse
		if err := s.agent.RPC(structs.CDCConsumerReadRPCMethod, &args, &reply); err != nil {
			return nil, err
		}
		if reply.Consumer == nil {
			return nil, CodedError(http.StatusNotFound, "no consumer of the stream "+stream)
		}
		return reply.Consumer, nil
	case http.MethodDelete:
		args := structs.CDCConsumerDeleteRequest{Stream: stream}
		s.parseWriteRequest(req, &args.WriteRequest)
		var reply structs.CDCConsumerWriteResponse
		if err := s.agent.RPC(structs.CDCConsumerDeleteRPCMethod, &args, &reply); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
}
