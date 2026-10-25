// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package mtls helps applications use the native mTLS of Norca: it loads the
// workload certificate, key and trust bundle that the client writes into the
// secrets directory of a task (and reloads them when they are renewed),
// builds tls.Config values that verify peers by their SPIFFE ID against the
// trust bundle, and authorizes connections with the service intentions of the
// namespace, which it keeps up to date with blocking queries.
//
// The package depends only on the standard library and the Nomad API client,
// so applications don't import any server code.
package mtls

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ID is the SPIFFE ID of a workload:
// spiffe://<trust-domain>/ns/<namespace>/job/<job>/group/<group>.
type ID struct {
	TrustDomain string
	Namespace   string
	Job         string
	Group       string
}

var (
	segmentRe     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	trustDomainRe = regexp.MustCompile(`^[a-z0-9._-]+$`)
)

// String returns the URI form of the ID.
func (id ID) String() string {
	return fmt.Sprintf("spiffe://%s/ns/%s/job/%s/group/%s", id.TrustDomain, id.Namespace, id.Job, id.Group)
}

// ParseID parses a workload SPIFFE ID.
func ParseID(raw string) (ID, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return ID{}, fmt.Errorf("invalid SPIFFE ID %q: %w", raw, err)
	}
	return ParseURL(u)
}

// ParseURL parses a workload SPIFFE ID given as a URL.
func ParseURL(u *url.URL) (ID, error) {
	if u == nil {
		return ID{}, errors.New("missing SPIFFE ID")
	}
	raw := u.String()
	if u.Scheme != "spiffe" {
		return ID{}, fmt.Errorf("invalid SPIFFE ID %q: scheme must be spiffe", raw)
	}
	if u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return ID{}, fmt.Errorf("invalid SPIFFE ID %q: user info, port, query and fragment are not allowed", raw)
	}
	if !trustDomainRe.MatchString(u.Host) {
		return ID{}, fmt.Errorf("invalid SPIFFE ID %q: invalid trust domain", raw)
	}
	if u.RawPath != "" {
		return ID{}, fmt.Errorf("invalid SPIFFE ID %q: percent-encoded path", raw)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 6 || parts[0] != "ns" || parts[2] != "job" || parts[4] != "group" {
		return ID{}, fmt.Errorf("invalid SPIFFE ID %q: path must be /ns/<namespace>/job/<job>/group/<group>", raw)
	}
	for _, i := range []int{1, 3, 5} {
		if parts[i] == "." || parts[i] == ".." || !segmentRe.MatchString(parts[i]) {
			return ID{}, fmt.Errorf("invalid SPIFFE ID %q: invalid path segment %q", raw, parts[i])
		}
	}
	return ID{TrustDomain: u.Host, Namespace: parts[1], Job: parts[3], Group: parts[5]}, nil
}

// IDFromCertificate returns the workload SPIFFE ID of a certificate, which
// must have exactly one URI SAN.
func IDFromCertificate(cert *x509.Certificate) (ID, error) {
	if cert == nil {
		return ID{}, errors.New("missing certificate")
	}
	if len(cert.URIs) != 1 {
		return ID{}, fmt.Errorf("certificate must have exactly one URI SAN, it has %d", len(cert.URIs))
	}
	return ParseURL(cert.URIs[0])
}

// dnsDomain is the DNS domain of the service names of workload
// certificates.
const dnsDomain = "nomad"

// ServicesFromCertificate returns the service names a workload certificate
// asserts in its DNS SANs: <service>.service.<namespace>.nomad,
// <service>.service.<namespace>.<region>.nomad and, in the default namespace,
// <service>.service.nomad. Only names of the namespace of the ID count.
func ServicesFromCertificate(cert *x509.Certificate, namespace string) []string {
	if cert == nil {
		return nil
	}
	ns := strings.ToLower(namespace)
	var services []string
	for _, name := range cert.DNSNames {
		labels := strings.Split(strings.ToLower(name), ".")
		if len(labels) < 3 || labels[1] != "service" || labels[len(labels)-1] != dnsDomain {
			continue
		}
		switch {
		case len(labels) == 3 && ns == "default":
		case len(labels) == 4 && labels[2] == ns:
		case len(labels) == 5 && labels[2] == ns:
		default:
			continue
		}
		service := labels[0]
		if service == "" {
			continue
		}
		dup := false
		for _, s := range services {
			if s == service {
				dup = true
				break
			}
		}
		if !dup {
			services = append(services, service)
		}
	}
	return services
}
