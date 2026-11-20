// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"net/url"
	"time"
)

// CertificateAuthority is an issuing CA of the built-in certificate issuer
// of a region, without its key.
type CertificateAuthority struct {
	ID           string
	Region       string
	Cert         string
	Chain        string
	Active       bool
	Imported     bool
	NotBefore    time.Time
	NotAfter     time.Time
	RotatedOutAt time.Time
	Revoked      []*CertificateRevocation
	CreateIndex  uint64
	ModifyIndex  uint64
}

// CertificateRevocation is a revoked certificate of an issuing CA.
type CertificateRevocation struct {
	Serial      string
	Certificate string
	RevokedAt   time.Time
	NotAfter    time.Time

	// Reason is the reason of the revocation, the name of an RFC 5280
	// CRLReason; empty for none (unspecified).
	Reason string
}

// CertificateRoots is the trust bundle of the issuing CA of a region.
type CertificateRoots struct {
	// ActiveID is the ID of the CA that signs new certificates.
	ActiveID string

	// Authorities are the CAs of the region, the active one first.
	Authorities []*CertificateAuthority

	// Roots is the PEM bundle a client of the certificates has to trust.
	Roots string
}

// CertificateCRL is a PEM encoded revocation list of an issuing CA.
type CertificateCRL struct {
	CRL string
}

// CertificateRevokeRequest is what a revocation of a certificate may say.
type CertificateRevokeRequest struct {
	// Reason is the reason of the revocation, the name of an RFC 5280
	// CRLReason: unspecified, keyCompromise, affiliationChanged,
	// superseded or cessationOfOperation; empty for none (unspecified).
	Reason string `json:",omitempty"`
}

// CertificateRevokeResponse is the answer to the revocation of a
// certificate.
type CertificateRevokeResponse struct {
	// Serial is the serial number that was revoked, in hexadecimal.
	Serial string
	WriteMeta
}

// CertificateAuthorityImportRequest brings a CA of the organisation.
type CertificateAuthorityImportRequest struct {
	// Cert is the PEM encoded CA certificate, followed by the certificates
	// above it if it is not self-signed, the root last.
	Cert string

	// Key is the PEM encoded private key of the CA.
	Key string
}

// CertificateAuthorityResponse is the answer to an import or a rotation of
// the issuing CA.
type CertificateAuthorityResponse struct {
	Authority *CertificateAuthority
	WriteMeta
}

// Certificates is a handle on the endpoints of the built-in certificate
// issuer. Certificates and issuers themselves are objects of the kinds
// norca.io/Certificate and norca.io/CertificateIssuer.
type Certificates struct {
	client *Client
}

// Certificates returns a handle on the endpoints of the built-in
// certificate issuer.
func (c *Client) Certificates() *Certificates {
	return &Certificates{client: c}
}

// Roots returns the trust bundle of the issuing CA of the region. It
// supports blocking queries, so callers can follow rotations.
func (c *Certificates) Roots(q *QueryOptions) (*CertificateRoots, *QueryMeta, error) {
	var resp CertificateRoots
	qm, err := c.client.query("/v1/certificates/roots", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// CRL returns the revocation list of an issuing CA of the region: of the
// active one if authority is empty.
func (c *Certificates) CRL(authority string, q *QueryOptions) (*CertificateCRL, *QueryMeta, error) {
	path := "/v1/certificates/crl"
	if authority != "" {
		path += "?authority=" + url.QueryEscape(authority)
	}
	var resp CertificateCRL
	qm, err := c.client.query(path, &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Revoke revokes the current certificate of a Certificate object of the
// namespace of the write options, for the reason of req if it gives one; a
// new one is issued. req may be nil.
func (c *Certificates) Revoke(name string, req *CertificateRevokeRequest, w *WriteOptions) (*CertificateRevokeResponse, error) {
	if name == "" {
		return nil, errors.New("missing certificate name")
	}
	var body any
	if req != nil {
		body = req
	}
	var resp CertificateRevokeResponse
	wm, err := c.client.put("/v1/certificate/"+url.PathEscape(name)+"/revoke", body, &resp, w)
	if err != nil {
		return nil, err
	}
	resp.WriteMeta = *wm
	return &resp, nil
}

// ImportAuthority puts a CA of the organisation in the place of the issuing
// CA of the region. It requires a management token.
func (c *Certificates) ImportAuthority(req *CertificateAuthorityImportRequest, w *WriteOptions) (*CertificateAuthorityResponse, error) {
	var resp CertificateAuthorityResponse
	wm, err := c.client.put("/v1/certificates/ca/import", req, &resp, w)
	if err != nil {
		return nil, err
	}
	resp.WriteMeta = *wm
	return &resp, nil
}

// RotateAuthority replaces the issuing CA of the region with a new one of
// the leader. It requires a management token.
func (c *Certificates) RotateAuthority(w *WriteOptions) (*CertificateAuthorityResponse, error) {
	var resp CertificateAuthorityResponse
	wm, err := c.client.put("/v1/certificates/ca/rotate", nil, &resp, w)
	if err != nil {
		return nil, err
	}
	resp.WriteMeta = *wm
	return &resp, nil
}
