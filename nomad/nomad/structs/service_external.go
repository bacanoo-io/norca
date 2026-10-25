// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
)

const (
	// ServiceRegistrationUpsertExternalRPCMethod registers or updates an
	// external service.
	//
	// Args: ExternalServiceUpsertRequest
	// Reply: ExternalServiceUpsertResponse
	ServiceRegistrationUpsertExternalRPCMethod = "ServiceRegistration.UpsertExternal"

	// ServiceRegistrationDeleteExternalRPCMethod deregisters an external
	// service.
	//
	// Args: ExternalServiceDeleteRequest
	// Reply: ExternalServiceDeleteResponse
	ServiceRegistrationDeleteExternalRPCMethod = "ServiceRegistration.DeleteExternal"

	// ServiceRegistrationUpdateExternalChecksRPCMethod updates the status of
	// checks of an external service: TTL renewals, and the results of the
	// checks run by the client node owning the registration.
	//
	// Args: ExternalServiceCheckUpdateRequest
	// Reply: ExternalServiceCheckUpdateResponse
	ServiceRegistrationUpdateExternalChecksRPCMethod = "ServiceRegistration.UpdateExternalChecks"

	// ServiceRegistrationListExternalRPCMethod lists external services.
	//
	// Args: ExternalServiceListRequest
	// Reply: ExternalServiceListResponse
	ServiceRegistrationListExternalRPCMethod = "ServiceRegistration.ListExternal"
)

const (
	// ExternalServiceKindAddress is an external service reached at an address
	// (IP address or host name) and port.
	ExternalServiceKindAddress = "address"

	// ExternalServiceKindAlias is an external service that is another DNS
	// name: the built-in DNS answers it with a CNAME record.
	ExternalServiceKindAlias = "alias"

	// ExternalServiceSourceAPI marks registrations made through the native API.
	ExternalServiceSourceAPI = "api"

	// ExternalServiceSourceConsul marks registrations made through the agent
	// endpoints of the Consul-compatible API.
	ExternalServiceSourceConsul = "consul"

	// ExternalCheckTypeTTL is a check whose status the registrant reports;
	// it turns critical when it is not renewed within its TTL.
	ExternalCheckTypeTTL = "ttl"

	// ExternalCheckTypeHTTP is an HTTP check run by the client node owning
	// the registration.
	ExternalCheckTypeHTTP = "http"

	// ExternalCheckTypeTCP is a TCP connect check run by the client node
	// owning the registration.
	ExternalCheckTypeTCP = "tcp"

	// ExternalCheckTTLExpiredOutput is the output of a TTL check that expired.
	ExternalCheckTTLExpiredOutput = "TTL expired"

	// MinExternalCheckTTL is the shortest TTL of a check.
	MinExternalCheckTTL = time.Second

	// MinExternalCheckInterval is the shortest interval of a check run by a
	// client node.
	MinExternalCheckInterval = time.Second

	// DefaultExternalCheckInterval is the interval of a check run by a client
	// node without one.
	DefaultExternalCheckInterval = 10 * time.Second

	// DefaultExternalCheckTimeout is the timeout of a check run by a client
	// node without one.
	DefaultExternalCheckTimeout = 10 * time.Second

	// DefaultExternalDeregisterCriticalAfter is how long an external service
	// registered through the native API stays critical before it is
	// deregistered, unless it sets its own period.
	DefaultExternalDeregisterCriticalAfter = time.Hour
)

// ServiceExternal holds the settings of an external service registration: a
// registration that is not tied to an allocation. It is registered by an
// operator or a self-registering application, and kept alive by TTL checks or
// by checks run by the client node that owns it.
type ServiceExternal struct {
	// Kind is address or alias. For aliases the Address of the registration
	// is the target DNS name.
	Kind string

	// Source is where the registration came from: api or consul.
	Source string

	// LocalID is the ID given to the registration at the client agent it was
	// registered with through the Consul-compatible API; the registration ID
	// is made unique across the nodes from it. Empty for the native API.
	LocalID string

	// Checks are the definitions of the checks of the registration. Their
	// states are in the Checks of the registration, matched by ID.
	Checks []*ExternalServiceCheck

	// DeregisterCriticalAfter is how long the registration may stay critical
	// before it is deregistered; zero keeps it.
	DeregisterCriticalAfter time.Duration
}

// ExternalServiceCheck is the definition of a check of an external service.
type ExternalServiceCheck struct {
	// ID of the check, unique within the registration (and, for checks
	// registered through the Consul-compatible API, within the node).
	ID string

	// Name of the check.
	Name string

	// Type is ttl, http or tcp.
	Type string

	// Notes are free text shown with the check.
	Notes string

	// TTL of a ttl check.
	TTL time.Duration

	// HTTP is the URL of an http check, Method its method (default GET),
	// Header its headers, and Body its body.
	HTTP   string
	Method string
	Header map[string][]string
	Body   string

	// TLSSkipVerify and TLSServerName configure https checks.
	TLSSkipVerify bool
	TLSServerName string

	// TCP is the host:port of a tcp check.
	TCP string

	// Interval and Timeout of http and tcp checks.
	Interval time.Duration
	Timeout  time.Duration

	// InitialStatus is the status of the check when it is created; critical
	// if empty.
	InitialStatus string
}

// Copy returns a deep copy of e.
func (e *ServiceExternal) Copy() *ServiceExternal {
	if e == nil {
		return nil
	}
	ne := *e
	if e.Checks != nil {
		ne.Checks = make([]*ExternalServiceCheck, len(e.Checks))
		for i, c := range e.Checks {
			ne.Checks[i] = c.Copy()
		}
	}
	return &ne
}

// Equal returns whether e and o are equal.
func (e *ServiceExternal) Equal(o *ServiceExternal) bool {
	if e == nil || o == nil {
		return e == o
	}
	if e.Kind != o.Kind || e.Source != o.Source || e.LocalID != o.LocalID ||
		e.DeregisterCriticalAfter != o.DeregisterCriticalAfter || len(e.Checks) != len(o.Checks) {
		return false
	}
	for i := range e.Checks {
		if !e.Checks[i].Equal(o.Checks[i]) {
			return false
		}
	}
	return true
}

// Check returns the definition of the check of the given ID, or nil.
func (e *ServiceExternal) Check(id string) *ExternalServiceCheck {
	if e == nil {
		return nil
	}
	for _, c := range e.Checks {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// HasTTLChecks returns whether the registration has a ttl check.
func (e *ServiceExternal) HasTTLChecks() bool {
	if e == nil {
		return false
	}
	for _, c := range e.Checks {
		if c.Type == ExternalCheckTypeTTL {
			return true
		}
	}
	return false
}

// HasNodeChecks returns whether the registration has a check run by a client
// node (http or tcp).
func (e *ServiceExternal) HasNodeChecks() bool {
	if e == nil {
		return false
	}
	for _, c := range e.Checks {
		if c.IsNodeCheck() {
			return true
		}
	}
	return false
}

// Copy returns a deep copy of c.
func (c *ExternalServiceCheck) Copy() *ExternalServiceCheck {
	if c == nil {
		return nil
	}
	nc := *c
	if c.Header != nil {
		nc.Header = make(map[string][]string, len(c.Header))
		for k, v := range c.Header {
			nc.Header[k] = append([]string(nil), v...)
		}
	}
	return &nc
}

// Equal returns whether c and o are equal.
func (c *ExternalServiceCheck) Equal(o *ExternalServiceCheck) bool {
	if c == nil || o == nil {
		return c == o
	}
	if c.ID != o.ID || c.Name != o.Name || c.Type != o.Type || c.Notes != o.Notes || c.TTL != o.TTL ||
		c.HTTP != o.HTTP || c.Method != o.Method || c.Body != o.Body || c.TLSSkipVerify != o.TLSSkipVerify ||
		c.TLSServerName != o.TLSServerName || c.TCP != o.TCP || c.Interval != o.Interval ||
		c.Timeout != o.Timeout || c.InitialStatus != o.InitialStatus {
		return false
	}
	return maps.EqualFunc(c.Header, o.Header, func(x, y []string) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	})
}

// IsNodeCheck returns whether the check is run by a client node.
func (c *ExternalServiceCheck) IsNodeCheck() bool {
	return c.Type == ExternalCheckTypeHTTP || c.Type == ExternalCheckTypeTCP
}

// Canonicalize sets the defaults of the check.
func (c *ExternalServiceCheck) Canonicalize() {
	c.Type = strings.ToLower(c.Type)
	if c.IsNodeCheck() {
		if c.Interval == 0 {
			c.Interval = DefaultExternalCheckInterval
		}
		if c.Timeout == 0 {
			c.Timeout = DefaultExternalCheckTimeout
		}
		if c.Timeout > c.Interval {
			c.Timeout = c.Interval
		}
	}
	if c.Type == ExternalCheckTypeHTTP && c.Method == "" {
		c.Method = "GET"
	}
}

// Validate validates the check.
func (c *ExternalServiceCheck) Validate() error {
	if c.ID == "" {
		return errors.New("check ID is required")
	}
	switch c.InitialStatus {
	case "", ServiceHealthPassing, ServiceHealthWarning, ServiceHealthCritical:
	default:
		return fmt.Errorf("check %q: invalid initial status %q", c.ID, c.InitialStatus)
	}
	switch c.Type {
	case ExternalCheckTypeTTL:
		if c.TTL < MinExternalCheckTTL {
			return fmt.Errorf("check %q: ttl must be at least %s", c.ID, MinExternalCheckTTL)
		}
	case ExternalCheckTypeHTTP:
		u, err := url.Parse(c.HTTP)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("check %q: invalid http URL %q", c.ID, c.HTTP)
		}
		if c.Interval < MinExternalCheckInterval {
			return fmt.Errorf("check %q: interval must be at least %s", c.ID, MinExternalCheckInterval)
		}
	case ExternalCheckTypeTCP:
		if _, _, err := net.SplitHostPort(c.TCP); err != nil {
			return fmt.Errorf("check %q: invalid tcp address %q: %v", c.ID, c.TCP, err)
		}
		if c.Interval < MinExternalCheckInterval {
			return fmt.Errorf("check %q: interval must be at least %s", c.ID, MinExternalCheckInterval)
		}
	default:
		return fmt.Errorf("check %q: unsupported check type %q (ttl, http or tcp)", c.ID, c.Type)
	}
	return nil
}

// validDNSName matches a DNS host name of one or more labels.
var validDNSName = regexp.MustCompile(`^(?i:[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?)(\.(?i:[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?))*\.?$`)

// IsExternal returns whether the registration is an external service.
func (s *ServiceRegistration) IsExternal() bool {
	return s != nil && s.External != nil
}

// ValidateExternal validates an external service registration.
func (s *ServiceRegistration) ValidateExternal() error {
	var mErr multierror.Error
	if s.External == nil {
		return errors.New("missing external service settings")
	}
	if s.ID == "" {
		mErr.Errors = append(mErr.Errors, errors.New("service ID is required"))
	}
	if err := (&Service{}).ValidateName(s.ServiceName); err != nil {
		mErr.Errors = append(mErr.Errors, err)
	}
	if s.Port < 0 || s.Port > 65535 {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("invalid port %d", s.Port))
	}
	if s.AllocID != "" || s.JobID != "" {
		mErr.Errors = append(mErr.Errors, errors.New("external services cannot belong to a job or allocation"))
	}

	switch s.External.Kind {
	case ExternalServiceKindAddress:
		if s.Address == "" {
			mErr.Errors = append(mErr.Errors, errors.New("address is required"))
		} else if net.ParseIP(s.Address) == nil && !validDNSName.MatchString(s.Address) {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("invalid address %q", s.Address))
		}
	case ExternalServiceKindAlias:
		if s.Address == "" {
			mErr.Errors = append(mErr.Errors, errors.New("alias target is required"))
		} else if net.ParseIP(s.Address) != nil || !validDNSName.MatchString(s.Address) {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("alias target %q must be a DNS name", s.Address))
		}
	default:
		mErr.Errors = append(mErr.Errors, fmt.Errorf("invalid kind %q (address or alias)", s.External.Kind))
	}
	if err := s.Validate(); err != nil {
		mErr.Errors = append(mErr.Errors, err)
	}
	if s.External.DeregisterCriticalAfter < 0 {
		mErr.Errors = append(mErr.Errors, errors.New("deregister_critical_after cannot be negative"))
	}

	seen := make(map[string]bool)
	for _, c := range s.External.Checks {
		if c == nil {
			mErr.Errors = append(mErr.Errors, errors.New("empty check"))
			continue
		}
		if seen[c.ID] {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("duplicate check ID %q", c.ID))
		}
		seen[c.ID] = true
		if err := c.Validate(); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	return mErr.ErrorOrNil()
}

// worseHealth returns the worse of two health states.
func worseHealth(a, b string) string {
	rank := func(s string) int {
		switch s {
		case ServiceHealthCritical:
			return 2
		case ServiceHealthWarning:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// SetExternalHealth sets Ready and the aggregate Health of an external
// registration from the states of its checks, and HealthUpdatedAt to now if
// the aggregate changed compared to previous (which may be nil).
func (s *ServiceRegistration) SetExternalHealth(previous *ServiceRegistration, now time.Time) {
	health := ServiceHealthPassing
	for _, c := range s.Checks {
		health = worseHealth(health, c.Status)
	}
	s.Ready = true
	s.Health = health
	if previous == nil || previous.Health != health || !previous.Ready {
		s.HealthUpdatedAt = now.UnixNano()
	} else {
		s.HealthUpdatedAt = previous.HealthUpdatedAt
	}
}

// CheckState returns the state of the check of the given ID, or nil.
func (s *ServiceRegistration) CheckState(id string) *ServiceRegistrationCheck {
	for _, c := range s.Checks {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// ExternalServiceUpsertRequest registers or updates an external service.
type ExternalServiceUpsertRequest struct {
	// Service is the registration. Its health fields are computed by the
	// servers; the states of checks that already exist are kept.
	Service *ServiceRegistration

	// NodeSecretID, if set, binds the registration to the client node of
	// that secret: the node runs its http and tcp checks, and the
	// registration is removed when the node goes down. Registrations with
	// http or tcp checks require it.
	NodeSecretID string

	WriteRequest
}

// ExternalServiceUpsertResponse is the response to an
// ExternalServiceUpsertRequest.
type ExternalServiceUpsertResponse struct {
	// ID of the registration.
	ID string
	WriteMeta
}

// ExternalServiceDeleteRequest deregisters an external service.
type ExternalServiceDeleteRequest struct {
	ID string
	WriteRequest
}

// ExternalServiceDeleteResponse is the response to an
// ExternalServiceDeleteRequest.
type ExternalServiceDeleteResponse struct {
	WriteMeta
}

// ExternalCheckUpdate is a status update of one check.
type ExternalCheckUpdate struct {
	// CheckID is the check updated; empty for every ttl check of the
	// registration.
	CheckID string

	// Status is passing, warning or critical.
	Status string

	// Output of the check.
	Output string
}

// ExternalServiceCheckUpdateRequest updates the status of checks of an
// external service.
type ExternalServiceCheckUpdateRequest struct {
	// ID of the registration.
	ID string

	Updates []*ExternalCheckUpdate

	// NodeReport marks the results of http and tcp checks reported by the
	// client node owning the registration, authenticated by its secret.
	NodeReport bool

	WriteRequest
}

// ExternalServiceCheckUpdateResponse is the response to an
// ExternalServiceCheckUpdateRequest.
type ExternalServiceCheckUpdateResponse struct {
	WriteMeta
}

// ExternalServiceListRequest lists external services.
type ExternalServiceListRequest struct {
	// NodeID, if set, limits the list to the registrations owned by the
	// client node.
	NodeID string

	QueryOptions
}

// ExternalServiceListResponse is the response to an
// ExternalServiceListRequest.
type ExternalServiceListResponse struct {
	Services []*ServiceRegistration
	QueryMeta
}
