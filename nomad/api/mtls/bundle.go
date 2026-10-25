// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtls

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Bundle is a trust bundle: the CA certificates of the regions, grouped by
// the trust domain each CA issues certificates for. A CA is only trusted for
// the trust domain in its own URI SAN (spiffe://<trust-domain>).
type Bundle struct {
	pools map[string]*x509.CertPool
	certs []*x509.Certificate
}

// ParseBundle parses a PEM trust bundle. CA certificates without a SPIFFE
// trust domain URI are ignored.
func ParseBundle(data []byte) (*Bundle, error) {
	b := &Bundle{pools: make(map[string]*x509.CertPool)}
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid certificate in trust bundle: %w", err)
		}
		if err := b.add(cert); err != nil {
			continue
		}
	}
	if len(b.certs) == 0 {
		return nil, errors.New("trust bundle contains no SPIFFE CA certificate")
	}
	return b, nil
}

func (b *Bundle) add(cert *x509.Certificate) error {
	if !cert.IsCA {
		return errors.New("not a CA certificate")
	}
	var td string
	for _, uri := range cert.URIs {
		if uri.Scheme == "spiffe" && (uri.Path == "" || uri.Path == "/") && trustDomainRe.MatchString(uri.Host) {
			td = uri.Host
			break
		}
	}
	if td == "" {
		return errors.New("CA certificate has no SPIFFE trust domain")
	}
	pool, ok := b.pools[td]
	if !ok {
		pool = x509.NewCertPool()
		b.pools[td] = pool
	}
	pool.AddCert(cert)
	b.certs = append(b.certs, cert)
	return nil
}

// TrustDomains returns the trust domains of the bundle.
func (b *Bundle) TrustDomains() []string {
	out := make([]string, 0, len(b.pools))
	for td := range b.pools {
		out = append(out, td)
	}
	slices.Sort(out)
	return out
}

// Certificates returns the CA certificates of the bundle.
func (b *Bundle) Certificates() []*x509.Certificate {
	return slices.Clone(b.certs)
}

// Peer is the verified identity of the other side of a connection.
type Peer struct {
	// ID is the SPIFFE ID of the peer.
	ID ID

	// Services are the services of the peer's task group that its
	// certificate asserts.
	Services []string

	// Certificate is the leaf certificate of the peer.
	Certificate *x509.Certificate
}

// Verify checks a certificate chain (leaf first) against the bundle: the
// leaf must be a valid workload certificate signed by a CA of its own trust
// domain.
func (b *Bundle) Verify(chain []*x509.Certificate) (*Peer, error) {
	return b.verifyAt(chain, time.Now())
}

func (b *Bundle) verifyAt(chain []*x509.Certificate, now time.Time) (*Peer, error) {
	if b == nil {
		return nil, errors.New("no trust bundle")
	}
	if len(chain) == 0 {
		return nil, errors.New("peer presented no certificate")
	}
	leaf := chain[0]
	if leaf.IsCA {
		return nil, errors.New("peer certificate is a CA certificate")
	}
	id, err := IDFromCertificate(leaf)
	if err != nil {
		return nil, err
	}
	pool, ok := b.pools[id.TrustDomain]
	if !ok {
		return nil, fmt.Errorf("trust domain %q of %s is not in the trust bundle", id.TrustDomain, id)
	}
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         pool,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return nil, fmt.Errorf("certificate of %s is not valid: %w", id, err)
	}
	return &Peer{
		ID:          id,
		Services:    ServicesFromCertificate(leaf, id.Namespace),
		Certificate: leaf,
	}, nil
}

// VerifyRaw parses and verifies a DER certificate chain, as passed to
// tls.Config.VerifyPeerCertificate.
func (b *Bundle) VerifyRaw(rawCerts [][]byte) (*Peer, error) {
	chain := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid peer certificate: %w", err)
		}
		chain = append(chain, cert)
	}
	return b.Verify(chain)
}
