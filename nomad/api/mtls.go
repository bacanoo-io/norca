// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"net/url"
	"time"
)

const (
	// MTLSModeApp means the application uses the workload certificate
	// itself.
	MTLSModeApp = "app"

	// MTLSModeSidecar means an injected Envoy sidecar terminates mTLS in
	// front of the service port, enforces the service intentions and
	// exposes the upstreams on local ports.
	MTLSModeSidecar = "sidecar"

	// MTLSModeGateway makes the service a cross-region gateway forwarding
	// TLS connections by SNI to the mTLS services of its region.
	MTLSModeGateway = "gateway"

	// TopicServiceIntention is the event stream topic of service intention
	// changes.
	TopicServiceIntention Topic = "ServiceIntention"

	// ServiceIntentionActionAllow and ServiceIntentionActionDeny are the
	// actions of a service intention.
	ServiceIntentionActionAllow = "allow"
	ServiceIntentionActionDeny  = "deny"

	// ServiceIntentionWildcard matches any service or namespace.
	ServiceIntentionWildcard = "*"
)

// ServiceMTLS is the mtls block of a service using the Nomad provider.
type ServiceMTLS struct {
	// Mode is "app" (default): the application uses the certificate
	// written into its secrets directory; "sidecar": an injected Envoy
	// task terminates and originates mTLS; "gateway": the service is a
	// cross-region SNI gateway.
	Mode string `mapstructure:"mode" hcl:"mode,optional"`

	// Upstreams are the services a sidecar proxy exposes locally (sidecar
	// mode only).
	Upstreams []*ServiceMTLSUpstream `mapstructure:"upstream" hcl:"upstream,block"`
}

// ServiceMTLSUpstream is an upstream block of a service mtls block.
type ServiceMTLSUpstream struct {
	Name    string `hcl:"name,label"`
	Service string `mapstructure:"service" hcl:"service,optional"`

	// Namespace of the service; empty for the namespace of the job.
	Namespace string `mapstructure:"namespace" hcl:"namespace,optional"`

	// Region of the service; empty for the region of the client. Another
	// region is reached through its gateway.
	Region string `mapstructure:"region" hcl:"region,optional"`

	// Gateway is the gateway service of Region, in its default namespace
	// (default "mtls-gateway").
	Gateway string `mapstructure:"gateway" hcl:"gateway,optional"`

	LocalPort int `mapstructure:"local_port" hcl:"local_port,optional"`
}

// Canonicalize sets the defaults of the mtls block.
func (m *ServiceMTLS) Canonicalize() {
	if m == nil {
		return
	}
	if m.Mode == "" {
		m.Mode = MTLSModeApp
	}
	if len(m.Upstreams) == 0 {
		m.Upstreams = nil
	}
}

// MTLSRoot is a certificate authority of the native mTLS trust bundle.
type MTLSRoot struct {
	ID           string
	Region       string
	TrustDomain  string
	RootCert     string
	Active       bool
	NotBefore    time.Time
	NotAfter     time.Time
	RotatedOutAt time.Time
	CreateIndex  uint64
	ModifyIndex  uint64
}

// MTLSRoots is the trust bundle of a region: its CAs and those of the
// federated regions.
type MTLSRoots struct {
	TrustDomain  string
	ActiveRootID string
	Roots        []*MTLSRoot
}

// MTLSRotateResponse is the result of a CA rotation.
type MTLSRotateResponse struct {
	RootID string
	WriteMeta
}

// MTLS is used to access the native mTLS endpoints.
type MTLS struct {
	client *Client
}

// MTLS returns a handle on the native mTLS endpoints.
func (c *Client) MTLS() *MTLS {
	return &MTLS{client: c}
}

// Roots returns the trust bundle of the region. It supports blocking
// queries, so callers can follow CA rotations.
func (m *MTLS) Roots(q *QueryOptions) (*MTLSRoots, *QueryMeta, error) {
	var resp MTLSRoots
	qm, err := m.client.query("/v1/mtls/roots", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// RotateCA replaces the active CA of the region. It requires a management
// token.
func (m *MTLS) RotateCA(w *WriteOptions) (*MTLSRotateResponse, error) {
	var resp MTLSRotateResponse
	wm, err := m.client.put("/v1/mtls/ca/rotate", nil, &resp, w)
	if err != nil {
		return nil, err
	}
	resp.WriteMeta = *wm
	return &resp, nil
}

// ServiceIntention allows or denies connections from a source service to a
// destination service of a namespace.
type ServiceIntention struct {
	// Namespace is the namespace of the destination service.
	Namespace string

	// Destination is the destination service, or "*".
	Destination string

	// SourceNamespace is the namespace of the source service (default:
	// Namespace), or "*".
	SourceNamespace string

	// Source is the source service, or "*".
	Source string

	// Action is "allow" or "deny".
	Action string

	Description string
	CreateIndex uint64
	ModifyIndex uint64
}

// ServiceIntentionEvent is the payload of ServiceIntention events.
type ServiceIntentionEvent struct {
	Intention *ServiceIntention
}

// ServiceIntentions is used to access the service intention endpoints.
type ServiceIntentions struct {
	client *Client
}

// ServiceIntentions returns a handle on the service intention endpoints.
func (c *Client) ServiceIntentions() *ServiceIntentions {
	return &ServiceIntentions{client: c}
}

// List returns the intentions of the namespace of the query options, or of
// every readable namespace with the "*" namespace. It supports blocking
// queries.
func (s *ServiceIntentions) List(q *QueryOptions) ([]*ServiceIntention, *QueryMeta, error) {
	var resp []*ServiceIntention
	qm, err := s.client.query("/v1/service-intentions", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

func intentionKeyQuery(sourceNamespace, source, destination string) string {
	v := url.Values{}
	v.Set("source", source)
	v.Set("destination", destination)
	if sourceNamespace != "" {
		v.Set("source_namespace", sourceNamespace)
	}
	return "/v1/service-intention?" + v.Encode()
}

// Info returns the intention from source (in sourceNamespace, which defaults
// to the destination namespace) to destination in the namespace of the query
// options.
func (s *ServiceIntentions) Info(sourceNamespace, source, destination string, q *QueryOptions) (*ServiceIntention, *QueryMeta, error) {
	if source == "" || destination == "" {
		return nil, nil, errors.New("source and destination are required")
	}
	var resp ServiceIntention
	qm, err := s.client.query(intentionKeyQuery(sourceNamespace, source, destination), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Upsert creates or replaces an intention. An intention without a namespace
// belongs to the namespace of the write options.
func (s *ServiceIntentions) Upsert(intention *ServiceIntention, w *WriteOptions) (*WriteMeta, error) {
	if intention == nil {
		return nil, errors.New("missing service intention")
	}
	return s.client.put("/v1/service-intention", intention, nil, w)
}

// Delete deletes an intention of the namespace of the write options.
func (s *ServiceIntentions) Delete(sourceNamespace, source, destination string, w *WriteOptions) (*WriteMeta, error) {
	if source == "" || destination == "" {
		return nil, errors.New("source and destination are required")
	}
	return s.client.delete(intentionKeyQuery(sourceNamespace, source, destination), nil, nil, w)
}
