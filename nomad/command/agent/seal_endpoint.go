// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"

	"github.com/hashicorp/nomad/nomad/structs"
)

// OperatorSealStatusRequest is GET /v1/operator/seal: the seal status of
// every server of the region, or of one (?server_id=).
func (s *HTTPServer) OperatorSealStatusRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(405, ErrInvalidMethod)
	}
	args := structs.SealStatusRequest{ServerID: req.URL.Query().Get("server_id")}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.SealStatusResponse
	if err := s.agent.RPC("Operator.SealStatus", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Servers == nil {
		out.Servers = []*structs.ServerSealStatus{}
	}
	return out, nil
}

// operatorUnsealBody is the body of POST /v1/operator/unseal.
type operatorUnsealBody struct {
	// Key is one unseal share, base64 encoded.
	Key   string
	Reset bool
}

// OperatorUnsealRequest is POST /v1/operator/unseal: one unseal share for
// a server (?server_id=, by default the server of this agent). The share is
// read from the body, never from the URL.
func (s *HTTPServer) OperatorUnsealRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return nil, CodedError(405, ErrInvalidMethod)
	}
	var body operatorUnsealBody
	if err := decodeBody(req, &body); err != nil {
		return nil, CodedError(400, err.Error())
	}
	args := structs.SealUnsealRequest{
		ServerID: req.URL.Query().Get("server_id"),
		Key:      body.Key,
		Reset:    body.Reset,
	}
	if args.ServerID == "" {
		srv := s.agent.Server()
		if srv == nil {
			return nil, CodedError(400, "server_id is required: this agent is not a server")
		}
		args.ServerID = srv.GetConfig().NodeName
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.SealUnsealResponse
	if err := s.agent.RPC("Operator.SealUnseal", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out, nil
}

// keyringRewrapRequest is POST /v1/operator/keyring/rewrap.
func (s *HTTPServer) keyringRewrapRequest(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	var args structs.KeyringRewrapRequest
	if req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0 {
		if err := decodeBody(req, &args); err != nil {
			return nil, CodedError(400, err.Error())
		}
	}
	if to := req.URL.Query().Get("to"); to != "" {
		args.To = to
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.KeyringRewrapResponse
	if err := s.agent.RPC("Keyring.Rewrap", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out, nil
}
