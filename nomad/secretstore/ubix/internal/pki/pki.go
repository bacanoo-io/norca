// Package pki is a small certificate-authority secrets engine: it generates an
// internal root CA (whose private key never leaves the vault) and issues
// short-lived leaf certificates constrained by named roles. The CA key and role
// definitions are stored through the barrier, so they are encrypted at rest.
//
// Scope: a self-signed root CA and leaf issuance with per-role domain and TTL
// constraints. Intermediate CAs and revocation/CRL are future work.
package pki

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// Errors.
var (
	ErrNoCA         = errors.New("pki: no CA configured (generate a root first)")
	ErrCAExists     = errors.New("pki: a CA is already configured")
	ErrRoleNotFound = errors.New("pki: role not found")
	ErrInvalidName  = errors.New("pki: invalid role name")
	ErrNotAllowed   = errors.New("pki: common name or SAN not permitted by the role")
	ErrInvalidKey   = errors.New("pki: unsupported key type or size")
)

// Storage is the subset of a backend the engine needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Engine is a PKI secrets engine mounted at a storage prefix.
type Engine struct {
	store  Storage
	prefix string
	now    func() time.Time
}

// New returns a PKI engine storing under prefix (e.g. "pki").
func New(store Storage, prefix string) *Engine {
	return &Engine{store: store, prefix: strings.Trim(prefix, "/"), now: func() time.Time { return time.Now().UTC() }}
}

func (e *Engine) caPath() string              { return e.prefix + "/ca" }
func (e *Engine) rolePath(name string) string { return e.prefix + "/role/" + name }

func validName(name string) bool { return name != "" && !strings.Contains(name, "/") }

// caBundle is the persisted CA: PEM cert plus its (secret) PEM private key.
type caBundle struct {
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
}

// Role constrains what certificates may be issued.
type Role struct {
	AllowedDomains  []string      `json:"allowed_domains"`
	AllowSubdomains bool          `json:"allow_subdomains"`
	MaxTTL          time.Duration `json:"max_ttl"`
	KeyType         string        `json:"key_type"` // "ec" (default) or "rsa"
	KeyBits         int           `json:"key_bits"` // ec: 256/384; rsa: 2048/4096
}

// RootConfig parameterizes root-CA generation.
type RootConfig struct {
	CommonName string
	TTL        time.Duration // default 10 years
	KeyType    string        // default "ec"
	KeyBits    int
}

// IssueRequest is a leaf-certificate request.
type IssueRequest struct {
	CommonName string
	AltNames   []string
	TTL        time.Duration
}

// IssuedCert is the result of [Engine.Issue].
type IssuedCert struct {
	CertificatePEM string `json:"certificate"`
	PrivateKeyPEM  string `json:"private_key"`
	IssuingCAPEM   string `json:"issuing_ca"`
	SerialNumber   string `json:"serial_number"`
	Expiration     string `json:"expiration"`
}

// GenerateRoot creates a self-signed root CA and stores it. It fails with
// [ErrCAExists] if one already exists.
func (e *Engine) GenerateRoot(ctx context.Context, cfg RootConfig) (string, error) {
	if entry, err := e.store.Get(ctx, e.caPath()); err != nil {
		return "", err
	} else if entry != nil {
		return "", ErrCAExists
	}

	priv, err := generateKey(cfg.KeyType, cfg.KeyBits)
	if err != nil {
		return "", err
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 10 * 365 * 24 * time.Hour
	}
	serial, err := randomSerial()
	if err != nil {
		return "", err
	}
	now := e.now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cfg.CommonName},
		NotBefore:             now.Add(-1 * time.Minute),
		NotAfter:              now.Add(ttl),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return "", fmt.Errorf("pki: create CA cert: %w", err)
	}
	certPEM := pemEncode("CERTIFICATE", der)
	keyPEM, err := marshalKeyPEM(priv)
	if err != nil {
		return "", err
	}
	blob, err := json.Marshal(caBundle{CertPEM: certPEM, KeyPEM: keyPEM})
	if err != nil {
		return "", fmt.Errorf("pki: marshal CA: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.caPath(), Value: blob}); err != nil {
		return "", fmt.Errorf("pki: persist CA: %w", err)
	}
	return certPEM, nil
}

// CACertPEM returns the CA certificate (public), or [ErrNoCA].
func (e *Engine) CACertPEM(ctx context.Context) (string, error) {
	ca, err := e.loadCA(ctx)
	if err != nil {
		return "", err
	}
	return ca.certPEM, nil
}

// WriteRole creates or replaces a role.
func (e *Engine) WriteRole(ctx context.Context, name string, role Role) error {
	if !validName(name) {
		return ErrInvalidName
	}
	blob, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("pki: marshal role: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.rolePath(name), Value: blob}); err != nil {
		return fmt.Errorf("pki: persist role: %w", err)
	}
	return nil
}

// ReadRole returns a role, or [ErrRoleNotFound].
func (e *Engine) ReadRole(ctx context.Context, name string) (*Role, error) {
	entry, err := e.store.Get(ctx, e.rolePath(name))
	if err != nil {
		return nil, fmt.Errorf("pki: read role: %w", err)
	}
	if entry == nil {
		return nil, ErrRoleNotFound
	}
	var role Role
	if err := json.Unmarshal(entry.Value, &role); err != nil {
		return nil, fmt.Errorf("pki: unmarshal role: %w", err)
	}
	return &role, nil
}

// ListRoles returns the role names.
func (e *Engine) ListRoles(ctx context.Context) ([]string, error) {
	names, err := e.store.List(ctx, e.prefix+"/role/")
	if err != nil {
		return nil, fmt.Errorf("pki: list roles: %w", err)
	}
	return names, nil
}

// DeleteRole removes a role.
func (e *Engine) DeleteRole(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if err := e.store.Delete(ctx, e.rolePath(name)); err != nil {
		return fmt.Errorf("pki: delete role: %w", err)
	}
	return nil
}

// Issue signs a leaf certificate for req under roleName's constraints.
func (e *Engine) Issue(ctx context.Context, roleName string, req IssueRequest) (*IssuedCert, error) {
	role, err := e.ReadRole(ctx, roleName)
	if err != nil {
		return nil, err
	}
	ca, err := e.loadCA(ctx)
	if err != nil {
		return nil, err
	}

	// Every requested name (CN + SANs) must be permitted by the role.
	names := append([]string{}, req.AltNames...)
	if req.CommonName != "" {
		names = append(names, req.CommonName)
	}
	for _, n := range names {
		if !domainAllowed(role, n) {
			return nil, ErrNotAllowed
		}
	}

	ttl := req.TTL
	if ttl <= 0 || (role.MaxTTL > 0 && ttl > role.MaxTTL) {
		if role.MaxTTL > 0 {
			ttl = role.MaxTTL
		} else {
			ttl = 24 * time.Hour
		}
	}

	leafKey, err := generateKey(role.KeyType, role.KeyBits)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := e.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: req.CommonName},
		DNSNames:     dedupe(names),
		NotBefore:    now.Add(-1 * time.Minute),
		NotAfter:     now.Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, leafKey.Public(), ca.key)
	if err != nil {
		return nil, fmt.Errorf("pki: sign leaf: %w", err)
	}
	keyPEM, err := marshalKeyPEM(leafKey)
	if err != nil {
		return nil, err
	}
	return &IssuedCert{
		CertificatePEM: pemEncode("CERTIFICATE", der),
		PrivateKeyPEM:  keyPEM,
		IssuingCAPEM:   ca.certPEM,
		SerialNumber:   serial.Text(16),
		Expiration:     tmpl.NotAfter.UTC().Format(time.RFC3339),
	}, nil
}

// --- helpers ---

type loadedCA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM string
}

func (e *Engine) loadCA(ctx context.Context) (*loadedCA, error) {
	entry, err := e.store.Get(ctx, e.caPath())
	if err != nil {
		return nil, fmt.Errorf("pki: read CA: %w", err)
	}
	if entry == nil {
		return nil, ErrNoCA
	}
	var b caBundle
	if err := json.Unmarshal(entry.Value, &b); err != nil {
		return nil, fmt.Errorf("pki: unmarshal CA: %w", err)
	}
	certBlock, _ := pem.Decode([]byte(b.CertPEM))
	keyBlock, _ := pem.Decode([]byte(b.KeyPEM))
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("pki: stored CA is malformed")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA cert: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("pki: CA key is not a signer")
	}
	return &loadedCA{cert: cert, key: signer, certPEM: b.CertPEM}, nil
}

// domainAllowed reports whether name is permitted by the role: an exact match
// against an allowed domain, or a subdomain of one when AllowSubdomains is set.
func domainAllowed(role *Role, name string) bool {
	for _, d := range role.AllowedDomains {
		if name == d {
			return true
		}
		if role.AllowSubdomains && strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

func generateKey(keyType string, bits int) (crypto.Signer, error) {
	switch keyType {
	case "", "ec":
		switch bits {
		case 0, 256:
			return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		case 384:
			return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		default:
			return nil, ErrInvalidKey
		}
	case "rsa":
		switch bits {
		case 0, 2048:
			return rsa.GenerateKey(rand.Reader, 2048)
		case 3072:
			return rsa.GenerateKey(rand.Reader, 3072)
		case 4096:
			return rsa.GenerateKey(rand.Reader, 4096)
		default:
			return nil, ErrInvalidKey
		}
	default:
		return nil, ErrInvalidKey
	}
}

func marshalKeyPEM(key crypto.Signer) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("pki: marshal key: %w", err)
	}
	return pemEncode("PRIVATE KEY", der), nil
}

func pemEncode(typ string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func randomSerial() (*big.Int, error) {
	// 128-bit random serial.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("pki: serial: %w", err)
	}
	return n, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
