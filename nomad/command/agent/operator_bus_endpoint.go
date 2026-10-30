// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"

	"github.com/hashicorp/nomad/nomad/structs"
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
