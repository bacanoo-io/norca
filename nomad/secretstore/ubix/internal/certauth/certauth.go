// Package certauth implements the TLS client-certificate auth method: a client
// presenting an mTLS certificate that chains to (or equals) a configured trusted
// certificate, and satisfies the role's name constraints, receives a token
// carrying the role's policies.
//
// Verification is done here, not by the TLS stack — the server requests a client
// certificate but does not verify it, so each cert role names its own trust
// anchor (a CA, or a specific self-signed client certificate).
package certauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// Errors.
var (
	ErrInvalidName   = errors.New("certauth: invalid cert role name")
	ErrCertNotFound  = errors.New("certauth: cert role not found")
	ErrDenied        = errors.New("certauth: no client certificate matched a configured role")
	ErrInvalidConfig = errors.New("certauth: a cert role needs a valid PEM certificate and at least one policy")
)

// CertRole is a trusted client-certificate role: any presented certificate that
// verifies against Certificate (a CA or a specific cert) and whose common name is
// permitted receives Policies.
type CertRole struct {
	Certificate        string        `json:"certificate"` // PEM: a CA, or a specific client cert to trust
	Policies           []string      `json:"policies"`
	AllowedCommonNames []string      `json:"allowed_common_names"` // empty = any CN accepted
	TokenTTL           time.Duration `json:"token_ttl"`
	TokenMaxTTL        time.Duration `json:"token_max_ttl"`
}

// Storage is the subset of a backend the method needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Method is the TLS certificate auth method.
type Method struct {
	store  Storage
	tokens *token.Store
	prefix string
}

// New returns a method storing under prefix (e.g. "auth/cert").
func New(store Storage, tokens *token.Store, prefix string) *Method {
	return &Method{store: store, tokens: tokens, prefix: strings.Trim(prefix, "/")}
}

func (m *Method) certKey(name string) string { return m.prefix + "/cert/" + name }

func validName(name string) bool { return name != "" && !strings.Contains(name, "/") }

// WriteCert creates or replaces a cert role.
func (m *Method) WriteCert(ctx context.Context, name string, role CertRole) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if len(role.Policies) == 0 {
		return ErrInvalidConfig
	}
	if _, err := parsePEMCert(role.Certificate); err != nil {
		return ErrInvalidConfig
	}
	blob, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("certauth: marshal role: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.certKey(name), Value: blob}); err != nil {
		return fmt.Errorf("certauth: persist role: %w", err)
	}
	return nil
}

// ReadCert returns a cert role, or [ErrCertNotFound].
func (m *Method) ReadCert(ctx context.Context, name string) (*CertRole, error) {
	entry, err := m.store.Get(ctx, m.certKey(name))
	if err != nil {
		return nil, fmt.Errorf("certauth: read role: %w", err)
	}
	if entry == nil {
		return nil, ErrCertNotFound
	}
	var role CertRole
	if err := json.Unmarshal(entry.Value, &role); err != nil {
		return nil, fmt.Errorf("certauth: unmarshal role: %w", err)
	}
	return &role, nil
}

// ListCerts returns the cert-role names.
func (m *Method) ListCerts(ctx context.Context) ([]string, error) {
	names, err := m.store.List(ctx, m.prefix+"/cert/")
	if err != nil {
		return nil, fmt.Errorf("certauth: list roles: %w", err)
	}
	return names, nil
}

// DeleteCert removes a cert role.
func (m *Method) DeleteCert(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if err := m.store.Delete(ctx, m.certKey(name)); err != nil {
		return fmt.Errorf("certauth: delete role: %w", err)
	}
	return nil
}

// Login validates the presented client-certificate chain (leaf first) against the
// configured cert roles and, on the first match, issues a token with that role's
// policies. Any failure is [ErrDenied].
func (m *Method) Login(ctx context.Context, presented []*x509.Certificate) (*token.Token, error) {
	if len(presented) == 0 {
		return nil, ErrDenied
	}
	leaf := presented[0]
	intermediates := x509.NewCertPool()
	for _, c := range presented[1:] {
		intermediates.AddCert(c)
	}

	names, err := m.ListCerts(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		role, err := m.ReadCert(ctx, name)
		if err != nil {
			continue
		}
		trusted, err := parsePEMCert(role.Certificate)
		if err != nil {
			continue
		}
		roots := x509.NewCertPool()
		roots.AddCert(trusted)
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			continue // doesn't chain to this role's trust anchor
		}
		if !cnAllowed(leaf.Subject.CommonName, role.AllowedCommonNames) {
			continue
		}
		cn := leaf.Subject.CommonName // the certificate CN is the identity alias name
		if role.TokenTTL > 0 {
			return m.tokens.CreateForLogin(ctx, role.Policies, role.TokenTTL, role.TokenMaxTTL, "cert", cn, nil)
		}
		return m.tokens.CreateForLogin(ctx, role.Policies, 0, role.TokenMaxTTL, "cert", cn, nil)
	}
	return nil, ErrDenied
}

// cnAllowed reports whether cn is permitted: an empty allow-list accepts any.
func cnAllowed(cn string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == cn {
			return true
		}
	}
	return false
}

func parsePEMCert(pemStr string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("certauth: not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
