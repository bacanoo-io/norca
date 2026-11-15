// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

// Certificates: TLS certificates for DNS names, kept issued and renewed by
// the leader and written to the secret store. Who signs is a
// CertificateIssuer: the issuing CA of the region, or an external issuer
// whose controller fulfils CertificateOrder objects. The three kinds belong
// to the group norca.io and are registered by the servers.

const (
	// OperatorsGroup is the group of the kinds the built-in operators of the
	// servers work on. It is the group of the node scaling kinds.
	OperatorsGroup = NodeScalingGroup

	CertificateIssuerKind = "CertificateIssuer"
	CertificateKind       = "Certificate"
	CertificateOrderKind  = "CertificateOrder"

	// OperatorsManagedBy is the managed_by value of objects the built-in
	// operators write themselves, and the reporter of their events.
	OperatorsManagedBy = "norca-operators"

	// OperatorsFinalizer is the finalizer of objects whose deletion has work
	// for a built-in operator.
	OperatorsFinalizer = "norca.io/operators"

	// Namespace metadata that says what certificates the namespace may ask
	// for. Nothing is allowed without them, except that an empty list of
	// issuers allows every issuer. The values are comma separated lists.
	NamespaceMetaCertificateDNSSuffixes = "norca.io/certificate-dns-suffixes"
	NamespaceMetaCertificateIPAddresses = "norca.io/certificate-ip-addresses"
	NamespaceMetaCertificateIssuers     = "norca.io/certificate-issuers"

	// SecretLabelCertificate is the label of a secret written for a
	// Certificate object: the name of the object.
	SecretLabelCertificate = "norca.io/certificate"

	// Items of the secret of a certificate.
	CertificateSecretItemCert    = "cert"
	CertificateSecretItemKey     = "key"
	CertificateSecretItemCA      = "ca"
	CertificateSecretItemNextKey = "next_key"

	// CertificateDefaultValidity and CertificateMaxValidity bound the
	// validity of certificates of the built-in issuer.
	CertificateDefaultValidity = 90 * 24 * time.Hour
	CertificateMaxValidity     = 365 * 24 * time.Hour

	// CertificateDefaultKeyType is the key type of a certificate that names
	// none.
	CertificateDefaultKeyType = "ecdsa-p256"

	// Conditions of certificates and issuers.
	CertificateConditionReady             = "Ready"
	CertificateConditionIssuing           = "Issuing"
	CertificateConditionRenewing          = "Renewing"
	CertificateConditionFailed            = "Failed"
	CertificateConditionIssuerUnavailable = "IssuerUnavailable"
	CertificateConditionDegraded          = "Degraded"

	// States of a certificate order.
	CertificateOrderPending    = "pending"
	CertificateOrderProcessing = "processing"
	CertificateOrderValid      = "valid"
	CertificateOrderFailed     = "failed"
)

// CertificateKeyTypes are the key types a certificate can ask for.
var CertificateKeyTypes = []string{"ecdsa-p256", "ecdsa-p384", "rsa-2048", "rsa-4096", "ed25519"}

// CertificateAuthority is an issuing CA of a region for Certificate objects
// of the built-in issuer: the active one, and retired ones that stay in the
// trust bundle until the certificates they signed have expired. It is not
// the CA of the native mTLS and never part of its trust bundle.
type CertificateAuthority struct {
	// ID is the UUID of the CA.
	ID string

	// Region is the region whose leader manages the CA.
	Region string

	// Cert is the PEM encoded CA certificate.
	Cert string

	// Chain holds the PEM encoded certificates above an imported CA that is
	// not self-signed, the root last. It is empty for a self-signed CA.
	Chain string

	// SigningKey is the PKCS #8 encoded private key of the CA, encrypted
	// with the root key SigningKeyID of the keyring. It never leaves the
	// servers.
	SigningKey []byte

	// SigningKeyID is the ID of the keyring root key that encrypts
	// SigningKey.
	SigningKeyID string

	// Active marks the CA that signs new certificates. A region has at most
	// one.
	Active bool

	// Imported marks a CA an operator brought: the leader never rotates it.
	Imported bool

	// NotBefore and NotAfter are the validity of the CA certificate.
	NotBefore time.Time
	NotAfter  time.Time

	// RotatedOutAt is the time the CA stopped being active.
	RotatedOutAt time.Time

	// Revoked are the certificates of the CA that were revoked and have not
	// expired yet.
	Revoked []*CertificateRevocation

	CreateIndex uint64
	ModifyIndex uint64
}

// CertificateRevocation is a revoked certificate of an issuing CA.
type CertificateRevocation struct {
	// Serial is the serial number, in hexadecimal.
	Serial string

	// Certificate is "namespace/name" of the Certificate object.
	Certificate string

	// RevokedAt is the time of the revocation, NotAfter the end of the
	// validity of the certificate: after it the entry is dropped.
	RevokedAt time.Time
	NotAfter  time.Time
}

// Copy returns a deep copy of a.
func (a *CertificateAuthority) Copy() *CertificateAuthority {
	if a == nil {
		return nil
	}
	na := *a
	na.SigningKey = slices.Clone(a.SigningKey)
	na.Revoked = make([]*CertificateRevocation, len(a.Revoked))
	for i, r := range a.Revoked {
		nr := *r
		na.Revoked[i] = &nr
	}
	if len(a.Revoked) == 0 {
		na.Revoked = nil
	}
	return &na
}

// Public returns a copy of a without the encrypted private key.
func (a *CertificateAuthority) Public() *CertificateAuthority {
	na := a.Copy()
	if na != nil {
		na.SigningKey = nil
		na.SigningKeyID = ""
	}
	return na
}

// CertificateAuthoritiesUpsertRequest upserts and deletes issuing CAs in one
// Raft entry, so a rotation (new active CA, retired old one) is atomic.
type CertificateAuthoritiesUpsertRequest struct {
	Authorities []*CertificateAuthority
	DeleteIDs   []string
	WriteRequest
}

// CertificateRootsRequest requests the trust bundle of the issuing CA of a
// region.
type CertificateRootsRequest struct {
	QueryOptions
}

// CertificateRootsResponse is the trust bundle of the issuing CA of a
// region. It never contains key material.
type CertificateRootsResponse struct {
	// ActiveID is the ID of the CA that signs new certificates.
	ActiveID string

	// Authorities are the CAs of the region, the active one first.
	Authorities []*CertificateAuthority

	// Roots is the PEM bundle a client of the certificates has to trust.
	Roots string

	QueryMeta
}

// CertificateCRLRequest requests the revocation list of an issuing CA: of
// the active one if Authority is empty.
type CertificateCRLRequest struct {
	Authority string
	QueryOptions
}

// CertificateCRLResponse carries a PEM encoded revocation list (RFC 5280).
type CertificateCRLResponse struct {
	CRL string
	QueryMeta
}

// CertificateRevokeRequest revokes the current certificate of a Certificate
// object; a new one is issued. The namespace is that of the request.
type CertificateRevokeRequest struct {
	Name string
	WriteRequest
}

// CertificateRevokeResponse is the answer to a revocation.
type CertificateRevokeResponse struct {
	// Serial is the serial number that was revoked, in hexadecimal.
	Serial string
	WriteMeta
}

// CertificateAuthorityImportRequest puts the CA of an operator in the place
// of the issuing CA of the region.
type CertificateAuthorityImportRequest struct {
	// Cert is the PEM encoded CA certificate, followed by the certificates
	// above it if it is not self-signed, the root last.
	Cert string

	// Key is the PEM encoded private key of the CA (PKCS #8, PKCS #1 or
	// SEC 1).
	Key string

	WriteRequest
}

// CertificateAuthorityRotateRequest replaces the issuing CA of the region
// with a new one of the leader.
type CertificateAuthorityRotateRequest struct {
	WriteRequest
}

// CertificateAuthorityResponse is the answer to an import or a rotation: the
// new active CA, without its key.
type CertificateAuthorityResponse struct {
	Authority *CertificateAuthority
	WriteMeta
}

// CertificateSpec is the spec of a Certificate object.
type CertificateSpec struct {
	Issuer       string   `json:"issuer"`
	DNSNames     []string `json:"dns_names"`
	IPAddresses  []string `json:"ip_addresses"`
	SecretPath   string   `json:"secret_path"`
	Validity     string   `json:"validity"`
	RenewBefore  string   `json:"renew_before"`
	KeyType      string   `json:"key_type"`
	DeleteSecret bool     `json:"delete_secret"`
}

// CertificateStatus is what the operator records in the status of a
// Certificate object. It holds nothing secret.
type CertificateStatus struct {
	Issuer        string `json:"issuer,omitempty"`
	Authority     string `json:"authority,omitempty"`
	Serial        string `json:"serial,omitempty"`
	NotBefore     string `json:"notBefore,omitempty"`
	NotAfter      string `json:"notAfter,omitempty"`
	RenewAt       string `json:"renewAt,omitempty"`
	SecretVersion uint64 `json:"secretVersion,omitempty"`

	// Request is the hash of what the current certificate was issued for:
	// a changed spec gives another one and a new certificate.
	Request string `json:"request,omitempty"`

	// Order is the name of the open order with an external issuer,
	// OrderRequest the hash it was placed for, Orders the number of orders
	// placed so far.
	Order        string `json:"order,omitempty"`
	OrderRequest string `json:"orderRequest,omitempty"`
	Orders       uint64 `json:"orders,omitempty"`

	// Revoke is the serial number an operator asked to revoke; the
	// certificate with it is replaced.
	Revoke string `json:"revoke,omitempty"`

	// Failures counts the attempts that failed in a row, RetryAt is when the
	// next one is made.
	Failures uint64 `json:"failures,omitempty"`
	RetryAt  string `json:"retryAt,omitempty"`
}

// CertificateIssuerSpec is the spec of a CertificateIssuer object.
type CertificateIssuerSpec struct {
	Builtin  *CertificateIssuerBuiltin  `json:"builtin,omitempty"`
	External *CertificateIssuerExternal `json:"external,omitempty"`
}

// CertificateIssuerBuiltin selects the issuing CA of the region.
type CertificateIssuerBuiltin struct{}

// CertificateIssuerExternal names the controller that fulfils the orders of
// the issuer. The configuration is the controller's; it is public (object
// events are not redacted), so it must hold no secret.
type CertificateIssuerExternal struct {
	Controller string         `json:"controller"`
	Config     map[string]any `json:"config,omitempty"`
}

// CertificateIssuerStatus is the status of an issuer the core reads. The
// controller of an external issuer writes it: the heartbeat, and the roots
// the chains it returns lead to.
type CertificateIssuerStatus struct {
	Heartbeat string `json:"heartbeat,omitempty"`
	Roots     string `json:"roots,omitempty"`
}

// CertificateOrderSpec is the spec of a CertificateOrder object, written by
// the servers.
type CertificateOrderSpec struct {
	Issuer      string   `json:"issuer"`
	Certificate string   `json:"certificate"`
	CSR         string   `json:"csr,omitempty"`
	DNSNames    []string `json:"dns_names,omitempty"`
	Revoke      string   `json:"revoke,omitempty"`
}

// CertificateOrderStatus is the status of a CertificateOrder object, written
// by the controller of the issuer.
type CertificateOrderStatus struct {
	State      string `json:"state,omitempty"`
	Chain      string `json:"chain,omitempty"`
	RenewAfter string `json:"renew_after,omitempty"`
	Problem    string `json:"problem,omitempty"`
}

// DecodeObjectPart decodes the spec or the status of an object into out.
func DecodeObjectPart(part map[string]any, out any) error {
	if len(part) == 0 {
		return nil
	}
	buf, err := json.Marshal(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, out)
}

// EncodeObjectPart encodes a typed spec or status as the map of an object.
func EncodeObjectPart(in any) (map[string]any, error) {
	buf, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ValidityDuration returns the validity the spec asks for, or the default.
func (s *CertificateSpec) ValidityDuration() (time.Duration, error) {
	if s.Validity == "" {
		return CertificateDefaultValidity, nil
	}
	d, err := time.ParseDuration(s.Validity)
	switch {
	case err != nil:
		return 0, fmt.Errorf("invalid validity %q: %v", s.Validity, err)
	case d < time.Hour:
		return 0, fmt.Errorf("validity %q is shorter than an hour", s.Validity)
	case d > CertificateMaxValidity:
		return 0, fmt.Errorf("validity %q is longer than the maximum, %s", s.Validity, CertificateMaxValidity)
	}
	return d, nil
}

// RenewBeforeDuration returns how long before the end of the validity the
// certificate is renewed, or 0 if the spec leaves it to the default.
func (s *CertificateSpec) RenewBeforeDuration() (time.Duration, error) {
	if s.RenewBefore == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s.RenewBefore)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid renew_before %q", s.RenewBefore)
	}
	return d, nil
}

// Validate checks what the schema of the kind can't.
func (s *CertificateSpec) Validate() error {
	if err := ValidateSecretPath(s.SecretPath); err != nil {
		return err
	}
	validity, err := s.ValidityDuration()
	if err != nil {
		return err
	}
	renewBefore, err := s.RenewBeforeDuration()
	if err != nil {
		return err
	}
	if renewBefore >= validity {
		return fmt.Errorf("renew_before %q is not shorter than the validity", s.RenewBefore)
	}
	if len(s.DNSNames) == 0 && len(s.IPAddresses) == 0 {
		return errors.New("a certificate needs a DNS name or an IP address")
	}
	seen := map[string]bool{}
	for _, name := range s.DNSNames {
		if err := ValidateCertificateDNSName(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("DNS name %q is listed twice", name)
		}
		seen[name] = true
	}
	for _, addr := range s.IPAddresses {
		if net.ParseIP(addr) == nil {
			return fmt.Errorf("invalid IP address %q", addr)
		}
		if seen[addr] {
			return fmt.Errorf("IP address %q is listed twice", addr)
		}
		seen[addr] = true
	}
	return nil
}

// RequestHash identifies what a certificate is issued for: the names, the
// key type, the issuer and the validity. The secret path and the renewal
// time are not part of it.
func (s *CertificateSpec) RequestHash() string {
	names := slices.Clone(s.DNSNames)
	slices.Sort(names)
	addrs := slices.Clone(s.IPAddresses)
	slices.Sort(addrs)
	keyType := s.KeyType
	if keyType == "" {
		keyType = CertificateDefaultKeyType
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		s.Issuer, strings.Join(names, ","), strings.Join(addrs, ","), keyType, s.Validity,
	}, "\n")))
	return hex.EncodeToString(sum[:16])
}

// ValidateCertificateDNSName checks a DNS name of a certificate: lower case
// labels, optionally a wildcard as the whole first label. A wildcard right
// under a top level name ("*.lab", "*.com") is refused: TLS clients do not
// match a name against it, so the certificate would be issued and of no
// use.
func ValidateCertificateDNSName(name string) error {
	host := strings.TrimPrefix(name, "*.")
	if host == "" || len(name) > 253 || host != strings.ToLower(host) {
		return fmt.Errorf("invalid DNS name %q", name)
	}
	if host != name && !strings.Contains(host, ".") {
		return fmt.Errorf("invalid DNS name %q: a wildcard right under a top level name is not accepted by TLS clients, put it one level lower (for example \"*.apps.%s\")", name, host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid DNS name %q", name)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
				return fmt.Errorf("invalid DNS name %q", name)
			}
		}
	}
	return nil
}

// SplitNamespaceMetaList splits a comma separated list of namespace
// metadata.
func SplitNamespaceMetaList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// CertificateDNSNameAllowed reports whether a DNS name is under one of the
// suffixes: the suffix itself or a name below it. A wildcard is allowed
// where the names it stands for are.
func CertificateDNSNameAllowed(name string, suffixes []string) bool {
	host := strings.TrimPrefix(name, "*.")
	for _, suffix := range suffixes {
		suffix = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(suffix), "."))
		if suffix == "" {
			continue
		}
		if strings.HasPrefix(name, "*.") {
			// "*.example.com" needs every name below example.com.
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}
	return false
}

// CertificateIPAllowed reports whether an IP address is one of the allowed
// addresses or inside one of the allowed networks.
func CertificateIPAllowed(addr string, allowed []string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	for _, item := range allowed {
		if _, network, err := net.ParseCIDR(item); err == nil {
			if network.Contains(ip) {
				return true
			}
			continue
		}
		if other := net.ParseIP(item); other != nil && other.Equal(ip) {
			return true
		}
	}
	return false
}

// CheckCertificateNames checks the names of a certificate against what the
// namespace may ask for. builtin says whether the issuer is the built-in
// one: only it signs IP addresses.
func CheckCertificateNames(spec *CertificateSpec, ns *Namespace, builtin bool) error {
	var meta map[string]string
	if ns != nil {
		meta = ns.Meta
	}
	if issuers := SplitNamespaceMetaList(meta[NamespaceMetaCertificateIssuers]); len(issuers) > 0 &&
		!slices.Contains(issuers, spec.Issuer) {
		return fmt.Errorf("the namespace may not use the issuer %q (%s)", spec.Issuer, NamespaceMetaCertificateIssuers)
	}
	suffixes := SplitNamespaceMetaList(meta[NamespaceMetaCertificateDNSSuffixes])
	for _, name := range spec.DNSNames {
		if !CertificateDNSNameAllowed(name, suffixes) {
			return fmt.Errorf("the namespace may not ask for the DNS name %q: it is under none of its suffixes (%s)",
				name, NamespaceMetaCertificateDNSSuffixes)
		}
	}
	if len(spec.IPAddresses) > 0 && !builtin {
		return errors.New("IP addresses are signed by the built-in issuer only")
	}
	addrs := SplitNamespaceMetaList(meta[NamespaceMetaCertificateIPAddresses])
	for _, addr := range spec.IPAddresses {
		if !CertificateIPAllowed(addr, addrs) {
			return fmt.Errorf("the namespace may not ask for the IP address %q (%s)",
				addr, NamespaceMetaCertificateIPAddresses)
		}
	}
	return nil
}

// CertificateOrderName returns the name of the n-th order of a certificate.
func CertificateOrderName(namespace, name string, n uint64) string {
	return fmt.Sprintf("%s.%s.%d", namespace, name, n)
}
