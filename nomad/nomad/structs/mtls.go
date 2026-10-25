// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
)

// Native mTLS (Consul independence plan, phase 5): every region runs its own
// certificate authority managed by the leader. Workloads whose task group has
// a service with an mtls block receive short-lived X.509 certificates with a
// SPIFFE ID derived from their workload identity, and ServiceIntention
// objects decide which services may connect to which.

const (
	// MTLSModeApp means the application uses the certificate itself.
	MTLSModeApp = "app"

	// MTLSModeSidecar means a sidecar proxy (Envoy) terminates mTLS for the
	// application. It is parsed but not implemented yet.
	MTLSModeSidecar = "sidecar"

	// DefaultMTLSDNSDomain is the DNS domain of the service names written into
	// workload certificates; it matches the default domain of the built-in
	// DNS server.
	DefaultMTLSDNSDomain = "nomad"
)

// ServiceMTLS is the mtls block of a service using the Nomad provider.
type ServiceMTLS struct {
	// Mode is "app" (default): the application uses the certificate itself.
	// "sidecar" is reserved for the Envoy sidecar and rejected for now.
	Mode string

	// Upstreams are the services the sidecar proxy exposes locally. They are
	// only meaningful in sidecar mode.
	Upstreams []*ServiceMTLSUpstream
}

// ServiceMTLSUpstream is an upstream block of a service mtls block.
type ServiceMTLSUpstream struct {
	// Name is the label of the block.
	Name string

	// Service is the name of the destination service.
	Service string

	// LocalPort is the port the sidecar listens on for the upstream.
	LocalPort int
}

// Copy returns a deep copy of m.
func (m *ServiceMTLS) Copy() *ServiceMTLS {
	if m == nil {
		return nil
	}
	nm := *m
	if m.Upstreams != nil {
		nm.Upstreams = make([]*ServiceMTLSUpstream, len(m.Upstreams))
		for i, u := range m.Upstreams {
			if u != nil {
				nu := *u
				nm.Upstreams[i] = &nu
			}
		}
	}
	return &nm
}

// Equal returns whether m and o are equal.
func (m *ServiceMTLS) Equal(o *ServiceMTLS) bool {
	if m == nil || o == nil {
		return m == o
	}
	if m.Mode != o.Mode {
		return false
	}
	return slices.EqualFunc(m.Upstreams, o.Upstreams, func(a, b *ServiceMTLSUpstream) bool {
		if a == nil || b == nil {
			return a == b
		}
		return *a == *b
	})
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

// Validate checks the mtls block.
func (m *ServiceMTLS) Validate() error {
	if m == nil {
		return nil
	}
	var mErr multierror.Error
	seen := make(map[string]struct{}, len(m.Upstreams))
	for _, u := range m.Upstreams {
		if u == nil {
			continue
		}
		if u.Name == "" {
			mErr.Errors = append(mErr.Errors, errors.New("mtls upstream requires a name"))
		} else if _, ok := seen[u.Name]; ok {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("mtls upstream %q is defined more than once", u.Name))
		}
		seen[u.Name] = struct{}{}
		if u.Service == "" {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("mtls upstream %q requires a service", u.Name))
		}
		if u.LocalPort < 0 || u.LocalPort > 65535 {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("mtls upstream %q local_port must be between 0 and 65535", u.Name))
		}
	}

	switch m.Mode {
	case "", MTLSModeApp:
		if len(m.Upstreams) > 0 {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("mtls upstream blocks are only used in mode %q, which is not implemented yet", MTLSModeSidecar))
		}
	case MTLSModeSidecar:
		mErr.Errors = append(mErr.Errors, fmt.Errorf("mtls mode %q is not implemented yet; use mode %q and let the application use the certificate", MTLSModeSidecar, MTLSModeApp))
	default:
		mErr.Errors = append(mErr.Errors, fmt.Errorf("mtls mode must be %q or %q, not %q", MTLSModeApp, MTLSModeSidecar, m.Mode))
	}
	return mErr.ErrorOrNil()
}

// serviceMTLSDiff returns the diff of two mtls blocks.
func serviceMTLSDiff(old, new *ServiceMTLS, contextual bool) *ObjectDiff {
	if old.Equal(new) {
		return nil
	}
	o, n := old, new
	if o == nil {
		o = &ServiceMTLS{}
	}
	if n == nil {
		n = &ServiceMTLS{}
	}
	diff := primitiveObjectDiff(o, n, []string{"Upstreams"}, "MTLS", contextual)
	if diff == nil {
		diff = &ObjectDiff{Type: DiffTypeNone, Name: "MTLS"}
	}
	oldUp := make(map[string]*ServiceMTLSUpstream, len(o.Upstreams))
	for _, u := range o.Upstreams {
		oldUp[u.Name] = u
	}
	newUp := make(map[string]*ServiceMTLSUpstream, len(n.Upstreams))
	for _, u := range n.Upstreams {
		newUp[u.Name] = u
	}
	for name, ou := range oldUp {
		var ud *ObjectDiff
		if nu, ok := newUp[name]; ok {
			ud = primitiveObjectDiff(ou, nu, nil, "Upstream", contextual)
		} else {
			ud = primitiveObjectDiff(ou, nil, nil, "Upstream", contextual)
		}
		if ud != nil {
			diff.Objects = append(diff.Objects, ud)
		}
	}
	for name, nu := range newUp {
		if _, ok := oldUp[name]; !ok {
			if ud := primitiveObjectDiff(nil, nu, nil, "Upstream", contextual); ud != nil {
				diff.Objects = append(diff.Objects, ud)
			}
		}
	}
	sort.Sort(ObjectDiffs(diff.Objects))
	if diff.Type == DiffTypeNone && (len(diff.Fields) > 0 || len(diff.Objects) > 0) {
		diff.Type = DiffTypeEdited
	}
	switch {
	case old == nil:
		diff.Type = DiffTypeAdded
	case new == nil:
		diff.Type = DiffTypeDeleted
	}
	return diff
}

// MTLSServices returns the services of the task group, group and task level,
// that have an mtls block.
func (tg *TaskGroup) MTLSServices() []*Service {
	if tg == nil {
		return nil
	}
	var out []*Service
	for _, s := range tg.Services {
		if s != nil && s.MTLS != nil {
			out = append(out, s)
		}
	}
	for _, t := range tg.Tasks {
		if t == nil {
			continue
		}
		for _, s := range t.Services {
			if s != nil && s.MTLS != nil {
				out = append(out, s)
			}
		}
	}
	return out
}

// UsesMTLS returns whether a service of the task group has an mtls block.
func (tg *TaskGroup) UsesMTLS() bool {
	return len(tg.MTLSServices()) > 0
}

// UsesMTLS returns whether a task group of the job uses native mTLS.
func (j *Job) UsesMTLS() bool {
	if j == nil {
		return false
	}
	for _, tg := range j.TaskGroups {
		if tg.UsesMTLS() {
			return true
		}
	}
	return false
}

// spiffeSegment matches the characters SPIFFE allows in a path segment.
var spiffeSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// spiffeTrustDomain matches the characters SPIFFE allows in a trust domain.
var spiffeTrustDomain = regexp.MustCompile(`^[a-z0-9._-]+$`)

// ValidateSPIFFESegment checks that a value can be used as a path segment of a
// SPIFFE ID.
func ValidateSPIFFESegment(kind, value string) error {
	if value == "." || value == ".." || !spiffeSegment.MatchString(value) {
		return fmt.Errorf("%s %q can't be used in a SPIFFE ID: only letters, digits, '.', '-' and '_' are allowed", kind, value)
	}
	return nil
}

// ValidateSPIFFETrustDomain checks a SPIFFE trust domain name.
func ValidateSPIFFETrustDomain(td string) error {
	if len(td) == 0 || len(td) > 255 || !spiffeTrustDomain.MatchString(td) {
		return fmt.Errorf("invalid trust domain %q: only lowercase letters, digits, '.', '-' and '_' are allowed", td)
	}
	return nil
}

// MTLSWorkloadSPIFFEID returns the SPIFFE ID of the workloads of a task group:
// spiffe://<trust-domain>/ns/<namespace>/job/<job>/group/<group>.
func MTLSWorkloadSPIFFEID(trustDomain, namespace, job, group string) string {
	return fmt.Sprintf("spiffe://%s/ns/%s/job/%s/group/%s", trustDomain, namespace, job, group)
}

// MTLSServiceDNSNames returns the DNS names of a service written into the
// certificates of its workloads, following the names of the built-in DNS
// server: <service>.service.<namespace>.<domain>,
// <service>.service.<namespace>.<region>.<domain> and, in the default
// namespace, <service>.service.<domain>.
func MTLSServiceDNSNames(service, namespace, region string) []string {
	service = strings.ToLower(service)
	names := []string{
		fmt.Sprintf("%s.service.%s.%s", service, namespace, DefaultMTLSDNSDomain),
		fmt.Sprintf("%s.service.%s.%s.%s", service, namespace, region, DefaultMTLSDNSDomain),
	}
	if namespace == DefaultNamespace {
		names = append(names, fmt.Sprintf("%s.service.%s", service, DefaultMTLSDNSDomain))
	}
	return names
}

// MTLSRoot is a certificate authority in the trust bundle of a region: the
// CAs of the region itself, current and retired, and the CAs replicated from
// the other regions of the federation.
type MTLSRoot struct {
	// ID is the UUID of the root.
	ID string

	// Region is the region whose leader manages the CA.
	Region string

	// TrustDomain is the SPIFFE trust domain the CA issues certificates for.
	TrustDomain string

	// RootCert is the PEM encoded self-signed CA certificate.
	RootCert string

	// SigningKey is the PKCS #8 encoded private key of the CA, encrypted
	// with the root key SigningKeyID of the keyring. Only the roots of the
	// region itself have it; it never leaves the servers.
	SigningKey []byte

	// SigningKeyID is the ID of the keyring root key that encrypts
	// SigningKey.
	SigningKeyID string

	// Active marks the CA that signs new certificates. Each region has at
	// most one active root.
	Active bool

	// NotBefore and NotAfter are the validity of the CA certificate.
	NotBefore time.Time
	NotAfter  time.Time

	// RotatedOutAt is the time the root stopped being active. A retired
	// root stays in the trust bundle until the certificates it signed have
	// expired.
	RotatedOutAt time.Time

	CreateIndex uint64
	ModifyIndex uint64
}

// Copy returns a deep copy of r.
func (r *MTLSRoot) Copy() *MTLSRoot {
	if r == nil {
		return nil
	}
	nr := *r
	nr.SigningKey = slices.Clone(r.SigningKey)
	return &nr
}

// Public returns a copy of r without the encrypted private key.
func (r *MTLSRoot) Public() *MTLSRoot {
	if r == nil {
		return nil
	}
	nr := *r
	nr.SigningKey = nil
	nr.SigningKeyID = ""
	return &nr
}

// MTLSRootsUpsertRequest upserts and deletes trust bundle roots in one Raft
// entry, so a CA rotation (new active root, retired old one) is atomic.
type MTLSRootsUpsertRequest struct {
	Roots     []*MTLSRoot
	DeleteIDs []string
	WriteRequest
}

// MTLSRootsRequest requests the trust bundle of a region.
type MTLSRootsRequest struct {
	QueryOptions
}

// MTLSRootsResponse is the trust bundle of a region. The roots never contain
// key material.
type MTLSRootsResponse struct {
	// TrustDomain is the trust domain of the active CA of the region.
	TrustDomain string

	// ActiveRootID is the ID of the CA that signs new certificates.
	ActiveRootID string

	// Roots are the CAs to trust: those of the region and of the federated
	// regions.
	Roots []*MTLSRoot

	QueryMeta
}

// MTLSSignRequest asks the servers to sign a workload certificate. The
// client sends the CSR of a key pair it generated itself, and the workload
// identity token of the task; the SPIFFE ID and the DNS names of the
// certificate are derived from the allocation, never from the CSR.
type MTLSSignRequest struct {
	AllocID  string
	TaskName string

	// IdentityToken is the workload identity (JWT) of the task.
	IdentityToken string

	// CSR is the PEM encoded certificate signing request.
	CSR string

	WriteRequest
}

// MTLSSignResponse holds a signed workload certificate.
type MTLSSignResponse struct {
	// CertPEM is the PEM encoded certificate.
	CertPEM string

	// SPIFFEID is the URI SAN of the certificate.
	SPIFFEID string

	// RootID is the ID of the CA that signed the certificate.
	RootID string

	NotBefore time.Time
	NotAfter  time.Time

	WriteMeta
}

// MTLSRotateRequest rotates the CA of a region.
type MTLSRotateRequest struct {
	WriteRequest
}

// MTLSRotateResponse is the result of a CA rotation.
type MTLSRotateResponse struct {
	// RootID is the ID of the new active CA.
	RootID string

	WriteMeta
}

// ServiceIntentionActionAllow and ServiceIntentionActionDeny are the actions
// of a service intention.
const (
	ServiceIntentionActionAllow = "allow"
	ServiceIntentionActionDeny  = "deny"

	// ServiceIntentionWildcard matches any service or namespace.
	ServiceIntentionWildcard = "*"

	// TopicServiceIntention is the event stream topic of service intention
	// changes.
	TopicServiceIntention Topic = "ServiceIntention"

	TypeServiceIntentionUpserted = "ServiceIntentionUpserted"
	TypeServiceIntentionDeleted  = "ServiceIntentionDeleted"
)

// ServiceIntention allows or denies connections from a source service to a
// destination service. It belongs to the namespace of the destination, which
// enforces it. The pair (source namespace, source, destination) is unique
// within the namespace.
type ServiceIntention struct {
	// Namespace is the namespace of the destination service.
	Namespace string

	// Destination is the destination service name, or "*" for every service
	// of the namespace.
	Destination string

	// SourceNamespace is the namespace of the source service; it defaults
	// to Namespace. "*" matches every namespace.
	SourceNamespace string

	// Source is the source service name, or "*" for every service.
	Source string

	// Action is "allow" or "deny".
	Action string

	// Description is a free-form description.
	Description string

	CreateIndex uint64
	ModifyIndex uint64
}

// Copy returns a copy of i.
func (i *ServiceIntention) Copy() *ServiceIntention {
	if i == nil {
		return nil
	}
	ni := *i
	return &ni
}

// Canonicalize sets the defaults of the intention.
func (i *ServiceIntention) Canonicalize() {
	if i.Namespace == "" {
		i.Namespace = DefaultNamespace
	}
	if i.SourceNamespace == "" {
		i.SourceNamespace = i.Namespace
	}
	i.Action = strings.ToLower(i.Action)
}

// Key returns a readable form of the unique key of the intention.
func (i *ServiceIntention) Key() string {
	return fmt.Sprintf("%s/%s -> %s/%s", i.SourceNamespace, i.Source, i.Namespace, i.Destination)
}

// SameKey returns whether i and o have the same unique key.
func (i *ServiceIntention) SameKey(o *ServiceIntention) bool {
	return i.Namespace == o.Namespace && i.Destination == o.Destination &&
		i.SourceNamespace == o.SourceNamespace && i.Source == o.Source
}

// Validate checks a canonicalized intention.
func (i *ServiceIntention) Validate() error {
	var mErr multierror.Error
	if i.Namespace == ServiceIntentionWildcard {
		mErr.Errors = append(mErr.Errors, errors.New("the namespace of an intention can't be a wildcard"))
	} else if !validNamespaceName.MatchString(i.Namespace) {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("invalid namespace %q", i.Namespace))
	}
	if i.SourceNamespace != ServiceIntentionWildcard && !validNamespaceName.MatchString(i.SourceNamespace) {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("invalid source namespace %q", i.SourceNamespace))
	}
	for _, f := range []struct{ name, value string }{{"source", i.Source}, {"destination", i.Destination}} {
		if f.value == "" {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("missing %s service", f.name))
		} else if f.value != ServiceIntentionWildcard && !validIntentionService.MatchString(f.value) {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("invalid %s service %q: must be a service name or %q", f.name, f.value, ServiceIntentionWildcard))
		}
	}
	switch i.Action {
	case ServiceIntentionActionAllow, ServiceIntentionActionDeny:
	default:
		mErr.Errors = append(mErr.Errors, fmt.Errorf("action must be %q or %q, not %q",
			ServiceIntentionActionAllow, ServiceIntentionActionDeny, i.Action))
	}
	if len(i.Description) > maxIntentionDescription {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("description is longer than %d bytes", maxIntentionDescription))
	}
	return mErr.ErrorOrNil()
}

const maxIntentionDescription = 1024

// validIntentionService matches service names (RFC 1123 labels, as service
// names of the Nomad provider).
var validIntentionService = regexp.MustCompile(`^(?i:[a-z0-9]|[a-z0-9][a-z0-9\-]{0,61}[a-z0-9])$`)

// IsValidMTLSServiceName returns whether a service name can be written into
// a certificate and used in an intention: an interpolated service name
// ("${...}") is not.
func IsValidMTLSServiceName(name string) bool {
	return validIntentionService.MatchString(name)
}

// ServiceIntentionListRequest lists the intentions of a namespace, or of
// every namespace with the "*" namespace.
type ServiceIntentionListRequest struct {
	QueryOptions
}

// ServiceIntentionListResponse holds intentions.
type ServiceIntentionListResponse struct {
	Intentions []*ServiceIntention
	QueryMeta
}

// ServiceIntentionGetRequest looks up one intention of the request
// namespace by its key.
type ServiceIntentionGetRequest struct {
	SourceNamespace string
	Source          string
	Destination     string
	QueryOptions
}

// SingleServiceIntentionResponse holds one intention or nil.
type SingleServiceIntentionResponse struct {
	Intention *ServiceIntention
	QueryMeta
}

// ServiceIntentionUpsertRequest creates or replaces intentions.
type ServiceIntentionUpsertRequest struct {
	Intentions []*ServiceIntention
	WriteRequest
}

// ServiceIntentionDeleteRequest deletes intentions. Only the key fields of
// the intentions are used.
type ServiceIntentionDeleteRequest struct {
	Intentions []*ServiceIntention
	WriteRequest
}

// ServiceIntentionEvent is the payload of ServiceIntention events.
type ServiceIntentionEvent struct {
	Intention *ServiceIntention
}
