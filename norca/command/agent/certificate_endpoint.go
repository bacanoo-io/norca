// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strings"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

func certificateAuthorityToAPI(ca *structs.CertificateAuthority) *api.CertificateAuthority {
	if ca == nil {
		return nil
	}
	out := &api.CertificateAuthority{
		ID:           ca.ID,
		Region:       ca.Region,
		Cert:         ca.Cert,
		Chain:        ca.Chain,
		Active:       ca.Active,
		Imported:     ca.Imported,
		NotBefore:    ca.NotBefore,
		NotAfter:     ca.NotAfter,
		RotatedOutAt: ca.RotatedOutAt,
		Revoked:      make([]*api.CertificateRevocation, 0, len(ca.Revoked)),
		CreateIndex:  ca.CreateIndex,
		ModifyIndex:  ca.ModifyIndex,
	}
	for _, r := range ca.Revoked {
		out.Revoked = append(out.Revoked, &api.CertificateRevocation{
			Serial:      r.Serial,
			Certificate: r.Certificate,
			RevokedAt:   r.RevokedAt,
			NotAfter:    r.NotAfter,
			Reason:      r.Reason,
		})
	}
	return out
}

// CertificateRootsRequest handles /v1/certificates/roots: the trust bundle
// of the issuing CA of the region.
func (s *HTTPServer) CertificateRootsRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.CertificateRootsRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.CertificateRootsResponse
	if err := s.agent.RPC("Certificate.Roots", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	roots := &api.CertificateRoots{
		ActiveID:    out.ActiveID,
		Authorities: make([]*api.CertificateAuthority, 0, len(out.Authorities)),
		Roots:       out.Roots,
	}
	for _, ca := range out.Authorities {
		roots.Authorities = append(roots.Authorities, certificateAuthorityToAPI(ca))
	}
	return roots, nil
}

// CertificateCRLRequest handles /v1/certificates/crl: the revocation list
// of an issuing CA of the region.
func (s *HTTPServer) CertificateCRLRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodGet {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.CertificateCRLRequest{Authority: req.URL.Query().Get("authority")}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	var out structs.CertificateCRLResponse
	if err := s.agent.RPC("Certificate.CRL", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	return &api.CertificateCRL{CRL: out.CRL}, nil
}

// CertificateSpecificRequest handles /v1/certificate/<name>/revoke. The
// body, api.CertificateRevokeRequest, is optional.
func (s *HTTPServer) CertificateSpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	name, found := strings.CutSuffix(strings.TrimPrefix(req.URL.Path, "/v1/certificate/"), "/revoke")
	if !found || name == "" || strings.Contains(name, "/") {
		return nil, CodedError(http.StatusNotFound, ErrInvalidMethod)
	}
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	var in api.CertificateRevokeRequest
	if req.ContentLength != 0 {
		if err := decodeBody(req, &in); err != nil {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}
	}
	args := structs.CertificateRevokeRequest{Name: name, Reason: in.Reason}
	s.parseWriteRequest(req, &args.WriteRequest)
	var out structs.CertificateRevokeResponse
	if err := s.agent.RPC("Certificate.Revoke", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &api.CertificateRevokeResponse{Serial: out.Serial}, nil
}

// CertificateAuthorityImportRequest handles /v1/certificates/ca/import.
func (s *HTTPServer) CertificateAuthorityImportRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	var in api.CertificateAuthorityImportRequest
	if err := decodeBody(req, &in); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	args := structs.CertificateAuthorityImportRequest{Cert: in.Cert, Key: in.Key}
	s.parseWriteRequest(req, &args.WriteRequest)
	var out structs.CertificateAuthorityResponse
	if err := s.agent.RPC("Certificate.ImportAuthority", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &api.CertificateAuthorityResponse{Authority: certificateAuthorityToAPI(out.Authority)}, nil
}

// CertificateAuthorityRotateRequest handles /v1/certificates/ca/rotate.
func (s *HTTPServer) CertificateAuthorityRotateRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	if req.Method != http.MethodPut && req.Method != http.MethodPost {
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
	args := structs.CertificateAuthorityRotateRequest{}
	s.parseWriteRequest(req, &args.WriteRequest)
	var out structs.CertificateAuthorityResponse
	if err := s.agent.RPC("Certificate.RotateAuthority", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &api.CertificateAuthorityResponse{Authority: certificateAuthorityToAPI(out.Authority)}, nil
}
