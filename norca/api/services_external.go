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

	// Source is api (native API), apply (declared in a file or edited as
	// HCL) or consul (Consul-compatible agent API).
	Source string

	// CheckedBy selects the client nodes that may run the checks.
	CheckedBy *ExternalCheckedBy

	// LocalID is the ID given at the client agent for registrations made
	// through the Consul-compatible API.
	LocalID string

	// Checks are the definitions of the checks.
	Checks []*ExternalServiceCheck

	// DeregisterCriticalAfter is how long the registration may stay critical
	// before it is deregistered; zero keeps it.
	DeregisterCriticalAfter time.Duration
}

// ExternalCheckedBy selects the client nodes that may run the http and tcp
// checks of an external service. The servers choose one of them and move the
// checks to another when it goes away.
type ExternalCheckedBy struct {
	// NodePool limits the nodes to a node pool; empty means every pool.
	NodePool string `json:",omitempty"`

	// Datacenters limits the nodes to datacenters (glob patterns); empty
	// means every datacenter.
	Datacenters []string `json:",omitempty"`
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

	// NodeID is the client node that owns the service and runs its http and
	// tcp checks (requires node write). Empty: the client agent the request
	// is sent to, if it has a client.
	NodeID string `json:",omitempty"`

	// Declared marks a service declared in a file or edited as HCL: it
	// is kept until deregistered (DeregisterCriticalAfter defaults to 0).
	Declared bool `json:",omitempty"`

	// CheckedBy lets the servers choose the client node that runs the http
	// and tcp checks; it excludes NodeID.
	CheckedBy *ExternalCheckedBy `json:",omitempty"`

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

// ExternalServiceItem is an external service of a file with the namespace it
// is declared in (empty: the namespace of the request).
type ExternalServiceItem struct {
	Namespace string `json:",omitempty"`
	Service   *ExternalService
}

// ExternalServiceProblem is an error of a file with its line (0 if unknown).
type ExternalServiceProblem struct {
	Message string
	Line    int `json:",omitempty"`
}

// ExternalServiceParseRequest is the text of a file of external services.
type ExternalServiceParseRequest struct {
	// Text is the HCL form of the file, or the JSON form if JSON is set.
	Text string
	JSON bool `json:",omitempty"`
}

// ExternalServiceParseResponse is the content of a file, or its problems.
type ExternalServiceParseResponse struct {
	Items    []*ExternalServiceItem
	Problems []*ExternalServiceProblem
}

// ExternalServiceFormatRequest asks for the HCL form of services.
type ExternalServiceFormatRequest struct {
	Items []*ExternalServiceItem
}

// ExternalServiceFormatResponse is the HCL form of services.
type ExternalServiceFormatResponse struct {
	Text string
}

// ParseExternal parses the text of a file of external services; a file with
// errors is not an error of the call but is returned as Problems.
func (s *Services) ParseExternal(req *ExternalServiceParseRequest, q *WriteOptions) (*ExternalServiceParseResponse, error) {
	var resp ExternalServiceParseResponse
	if _, err := s.client.post("/v1/service/external/parse", req, &resp, q); err != nil {
		return nil, err
	}
	return &resp, nil
}

// FormatExternal returns the HCL form of external services.
func (s *Services) FormatExternal(items []*ExternalServiceItem, q *WriteOptions) (string, error) {
	var resp ExternalServiceFormatResponse
	if _, err := s.client.post("/v1/service/external/format",
		&ExternalServiceFormatRequest{Items: items}, &resp, q); err != nil {
		return "", err
	}
	return resp.Text, nil
}
