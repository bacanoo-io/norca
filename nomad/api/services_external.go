// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"net/url"
	"time"
)

const (
	// ExternalServiceKindAddress is an external service reached at an
	// address (IP address or host name) and port.
	ExternalServiceKindAddress = "address"

	// ExternalServiceKindAlias is an external service that is another DNS
	// name; the built-in DNS answers it with a CNAME record.
	ExternalServiceKindAlias = "alias"

	// ExternalCheckTypeTTL is a check renewed by the registrant.
	ExternalCheckTypeTTL = "ttl"

	// ExternalCheckTypeHTTP is an HTTP check run by the client agent the
	// service was registered through.
	ExternalCheckTypeHTTP = "http"

	// ExternalCheckTypeTCP is a TCP connect check run by the client agent
	// the service was registered through.
	ExternalCheckTypeTCP = "tcp"
)

// ServiceExternal is set on the registrations of external services.
type ServiceExternal struct {
	// Kind is address or alias.
	Kind string

	// Source is api (native API) or consul (Consul-compatible agent API).
	Source string

	// LocalID is the ID given at the client agent for registrations made
	// through the Consul-compatible API.
	LocalID string

	// Checks are the definitions of the checks.
	Checks []*ExternalServiceCheck

	// DeregisterCriticalAfter is how long the registration may stay critical
	// before it is deregistered; zero keeps it.
	DeregisterCriticalAfter time.Duration
}

// ExternalService is the registration of a service that is not tied to an
// allocation: a machine outside the cluster, a managed database, or a DNS
// alias.
type ExternalService struct {
	// ID of the registration; defaults to the name.
	ID string `json:",omitempty"`

	// Name of the service.
	Name string

	// Kind is address (default) or alias.
	Kind string `json:",omitempty"`

	// Address is the IP address or host name of an address service.
	Address string `json:",omitempty"`

	// Target is the DNS name an alias points to.
	Target string `json:",omitempty"`

	// Port of the service.
	Port int `json:",omitempty"`

	Tags []string          `json:",omitempty"`
	Meta map[string]string `json:",omitempty"`

	// Datacenter the service is reported in; by default the datacenter of
	// the client agent owning it, or none.
	Datacenter string `json:",omitempty"`

	// Checks of the service. Without a TTL check, and unless it has http or
	// tcp checks run by a client agent, the registration is static and
	// requires operator write.
	Checks []*ExternalServiceCheck `json:",omitempty"`

	// DeregisterCriticalAfter is how long the service may stay critical
	// before it is deregistered. Nil uses the default (1h); zero keeps it.
	DeregisterCriticalAfter *time.Duration `json:",omitempty"`
}

// ExternalServiceCheck is a check of an external service.
type ExternalServiceCheck struct {
	// ID of the check; defaults to the type, numbered if needed.
	ID string `json:",omitempty"`

	Name string `json:",omitempty"`

	// Type is ttl, http or tcp.
	Type string

	Notes string `json:",omitempty"`

	// TTL of a ttl check.
	TTL time.Duration `json:",omitempty"`

	// HTTP is the URL of an http check.
	HTTP          string              `json:",omitempty"`
	Method        string              `json:",omitempty"`
	Header        map[string][]string `json:",omitempty"`
	Body          string              `json:",omitempty"`
	TLSSkipVerify bool                `json:",omitempty"`
	TLSServerName string              `json:",omitempty"`

	// TCP is the host:port of a tcp check.
	TCP string `json:",omitempty"`

	// Interval and Timeout of http and tcp checks.
	Interval time.Duration `json:",omitempty"`
	Timeout  time.Duration `json:",omitempty"`

	// InitialStatus of the check; passing by default for registrations
	// through the native API.
	InitialStatus string `json:",omitempty"`
}

// ExternalCheckUpdate is a status update of the TTL checks of an external
// service.
type ExternalCheckUpdate struct {
	// CheckID is the check updated; empty for every TTL check.
	CheckID string `json:",omitempty"`

	// Status is passing (default), warning or critical.
	Status string `json:",omitempty"`

	// Output of the check.
	Output string `json:",omitempty"`
}

// ExternalServiceRegisterResponse is the response to a registration.
type ExternalServiceRegisterResponse struct {
	ID    string
	Index uint64
}

// RegisterExternal registers or updates an external service.
func (s *Services) RegisterExternal(svc *ExternalService, q *WriteOptions) (string, *WriteMeta, error) {
	var resp ExternalServiceRegisterResponse
	wm, err := s.client.put("/v1/service/external/"+url.PathEscape(svc.Name), svc, &resp, q)
	if err != nil {
		return "", nil, err
	}
	return resp.ID, wm, nil
}

// UpdateExternalTTL renews or updates the TTL checks of an external service.
func (s *Services) UpdateExternalTTL(id string, update *ExternalCheckUpdate, q *WriteOptions) (*WriteMeta, error) {
	if update == nil {
		update = &ExternalCheckUpdate{}
	}
	return s.client.put("/v1/service/external/"+url.PathEscape(id)+"/ttl", update, nil, q)
}

// DeregisterExternal deregisters an external service.
func (s *Services) DeregisterExternal(id string, q *WriteOptions) (*WriteMeta, error) {
	return s.client.delete("/v1/service/external/"+url.PathEscape(id), nil, nil, q)
}

// ListExternal lists the external services of the namespace.
func (s *Services) ListExternal(q *QueryOptions) ([]*ServiceRegistration, *QueryMeta, error) {
	var resp []*ServiceRegistration
	qm, err := s.client.query("/v1/services/external", &resp, q)
	if err != nil {
		return nil, qm, err
	}
	return resp, qm, nil
}
