// Package jwtauth implements the JWT/OIDC auth method: a client presents a
// signed JWT and, if it verifies against the configured keys and satisfies a
// role's claim bindings, receives a token carrying the role's policies.
//
// Signatures are verified with the standard library (RS256/384/512 and
// ES256/384/512) against static PEM public keys and/or a fetched JWKS — no
// dependency. The JWKS URL may be configured directly or resolved from an OIDC
// issuer's .well-known/openid-configuration (OIDC discovery).
package jwtauth

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// Errors.
var (
	ErrInvalidName   = errors.New("jwtauth: invalid role name")
	ErrRoleNotFound  = errors.New("jwtauth: role not found")
	ErrDenied        = errors.New("jwtauth: token rejected")
	ErrNotConfigured = errors.New("jwtauth: not configured")
	ErrInvalidConfig = errors.New("jwtauth: config needs a JWKS URL, an OIDC discovery URL, or validation keys; a role needs a policy")
)

// Config holds the signature-validation settings. Provide one of: a JWKS URL,
// an OIDC discovery URL (the issuer — the JWKS URL is resolved from its
// .well-known/openid-configuration), or static validation public keys.
type Config struct {
	JWKSURL           string   `json:"jwks_url"`
	OIDCDiscoveryURL  string   `json:"oidc_discovery_url"`
	ValidationPubKeys []string `json:"jwt_validation_pubkeys"` // PEM
	BoundIssuer       string   `json:"bound_issuer"`
	GroupsClaim       string   `json:"groups_claim"` // claim holding the caller's group names, for identity external groups
}

// Role binds JWT claims to policies.
type Role struct {
	BoundAudiences []string          `json:"bound_audiences"`
	BoundClaims    map[string]string `json:"bound_claims"` // claim -> required value
	Policies       []string          `json:"policies"`
	TokenTTL       time.Duration     `json:"token_ttl"`
	TokenMaxTTL    time.Duration     `json:"token_max_ttl"`
}

// Storage is the subset of a backend the method needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Method is the JWT/OIDC auth method.
type Method struct {
	store  Storage
	tokens *token.Store
	prefix string
	fetch  func(ctx context.Context, url string) ([]byte, error) // JWKS fetch (injectable)

	mu              sync.Mutex
	jwksURL         string
	jwksCache       []crypto.PublicKey
	discoveryURL    string // the OIDC discovery URL last resolved
	resolvedJWKSURL string // jwks_uri resolved from discoveryURL
}

// New returns a method storing under prefix (e.g. "auth/jwt").
func New(store Storage, tokens *token.Store, prefix string) *Method {
	return &Method{store: store, tokens: tokens, prefix: strings.Trim(prefix, "/"), fetch: httpFetch}
}

func (m *Method) configKey() string          { return m.prefix + "/config" }
func (m *Method) roleKey(name string) string { return m.prefix + "/role/" + name }

func validName(name string) bool { return name != "" && !strings.Contains(name, "/") }

// Configure stores the validation config.
func (m *Method) Configure(ctx context.Context, cfg Config) error {
	if cfg.JWKSURL == "" && cfg.OIDCDiscoveryURL == "" && len(cfg.ValidationPubKeys) == 0 {
		return ErrInvalidConfig
	}
	// Validate any static keys up front.
	for _, pem := range cfg.ValidationPubKeys {
		if _, err := parsePEMPublicKey(pem); err != nil {
			return fmt.Errorf("jwtauth: validation key: %w", err)
		}
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("jwtauth: marshal config: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.configKey(), Value: blob}); err != nil {
		return fmt.Errorf("jwtauth: persist config: %w", err)
	}
	m.mu.Lock()
	m.jwksCache = nil      // force refetch
	m.resolvedJWKSURL = "" // force OIDC re-discovery
	m.mu.Unlock()
	return nil
}

// Reset drops the cached JWKS keys and OIDC discovery result, so the next login
// fetches them again. Called when the vault is sealed.
func (m *Method) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jwksURL, m.jwksCache = "", nil
	m.discoveryURL, m.resolvedJWKSURL = "", ""
}

func (m *Method) config(ctx context.Context) (*Config, error) {
	entry, err := m.store.Get(ctx, m.configKey())
	if err != nil {
		return nil, fmt.Errorf("jwtauth: read config: %w", err)
	}
	if entry == nil {
		return nil, ErrNotConfigured
	}
	var cfg Config
	if err := json.Unmarshal(entry.Value, &cfg); err != nil {
		return nil, fmt.Errorf("jwtauth: unmarshal config: %w", err)
	}
	return &cfg, nil
}

// WriteRole creates or replaces a role.
func (m *Method) WriteRole(ctx context.Context, name string, role Role) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if len(role.Policies) == 0 {
		return ErrInvalidConfig
	}
	blob, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("jwtauth: marshal role: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.roleKey(name), Value: blob}); err != nil {
		return fmt.Errorf("jwtauth: persist role: %w", err)
	}
	return nil
}

// ReadRole returns a role, or [ErrRoleNotFound].
func (m *Method) ReadRole(ctx context.Context, name string) (*Role, error) {
	entry, err := m.store.Get(ctx, m.roleKey(name))
	if err != nil {
		return nil, fmt.Errorf("jwtauth: read role: %w", err)
	}
	if entry == nil {
		return nil, ErrRoleNotFound
	}
	var role Role
	if err := json.Unmarshal(entry.Value, &role); err != nil {
		return nil, fmt.Errorf("jwtauth: unmarshal role: %w", err)
	}
	return &role, nil
}

// ListRoles returns the role names.
func (m *Method) ListRoles(ctx context.Context) ([]string, error) {
	names, err := m.store.List(ctx, m.prefix+"/role/")
	if err != nil {
		return nil, fmt.Errorf("jwtauth: list roles: %w", err)
	}
	return names, nil
}

// DeleteRole removes a role.
func (m *Method) DeleteRole(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if err := m.store.Delete(ctx, m.roleKey(name)); err != nil {
		return fmt.Errorf("jwtauth: delete role: %w", err)
	}
	return nil
}

// Login verifies rawJWT against the config and roleName's bindings and, on
// success, issues a token with the role's policies. Any failure is [ErrDenied].
func (m *Method) Login(ctx context.Context, roleName, rawJWT string) (*token.Token, error) {
	cfg, err := m.config(ctx)
	if err != nil {
		return nil, err
	}
	role, err := m.ReadRole(ctx, roleName)
	if err != nil {
		return nil, err
	}

	header, claims, signingInput, sig, err := splitJWT(rawJWT)
	if err != nil {
		return nil, ErrDenied
	}
	alg, _ := header["alg"].(string)
	if alg == "" {
		return nil, ErrDenied
	}

	if !m.verifySignature(ctx, cfg, alg, signingInput, sig) {
		return nil, ErrDenied
	}
	if err := validateClaims(cfg, role, claims); err != nil {
		return nil, ErrDenied
	}

	subject, _ := claims["sub"].(string) // the JWT subject is the identity alias name
	groups := extractGroups(claims, cfg.GroupsClaim)
	if role.TokenTTL > 0 {
		return m.tokens.CreateForLogin(ctx, role.Policies, role.TokenTTL, role.TokenMaxTTL, "jwt", subject, groups)
	}
	return m.tokens.CreateForLogin(ctx, role.Policies, 0, role.TokenMaxTTL, "jwt", subject, groups)
}

// extractGroups reads the group-name list from the named claim. It accepts a
// JSON array of strings (the usual OIDC shape) or a single string; anything else
// yields no groups. An empty claim name disables group extraction.
func extractGroups(claims map[string]any, claim string) []string {
	if claim == "" {
		return nil
	}
	switch v := claims[claim].(type) {
	case []any:
		var out []string
		for _, g := range v {
			if s, ok := g.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

// verifySignature tries the static keys and the JWKS keys; on a miss it refetches
// the JWKS once (to pick up rotated keys) before giving up.
func (m *Method) verifySignature(ctx context.Context, cfg *Config, alg string, signingInput, sig []byte) bool {
	tryKeys := func(keys []crypto.PublicKey) bool {
		for _, k := range keys {
			if verify(alg, k, signingInput, sig) == nil {
				return true
			}
		}
		return false
	}

	var static []crypto.PublicKey
	for _, pem := range cfg.ValidationPubKeys {
		if k, err := parsePEMPublicKey(pem); err == nil {
			static = append(static, k)
		}
	}
	if tryKeys(static) {
		return true
	}
	jwksURL := m.resolveJWKSURL(ctx, cfg)
	if jwksURL == "" {
		return false
	}
	if tryKeys(m.jwksKeys(ctx, jwksURL, false)) {
		return true
	}
	return tryKeys(m.jwksKeys(ctx, jwksURL, true)) // force refetch
}

// resolveJWKSURL returns the JWKS URL to use: cfg.JWKSURL if set, otherwise the
// jwks_uri discovered from cfg.OIDCDiscoveryURL's
// /.well-known/openid-configuration (fetched once and cached). Returns "" if
// neither is configured or discovery fails.
func (m *Method) resolveJWKSURL(ctx context.Context, cfg *Config) string {
	if cfg.JWKSURL != "" {
		return cfg.JWKSURL
	}
	if cfg.OIDCDiscoveryURL == "" {
		return ""
	}
	m.mu.Lock()
	if m.discoveryURL == cfg.OIDCDiscoveryURL && m.resolvedJWKSURL != "" {
		u := m.resolvedJWKSURL
		m.mu.Unlock()
		return u
	}
	m.mu.Unlock()

	doc := strings.TrimRight(cfg.OIDCDiscoveryURL, "/") + "/.well-known/openid-configuration"
	data, err := m.fetch(ctx, doc)
	if err != nil {
		return ""
	}
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(data, &meta); err != nil || meta.JWKSURI == "" {
		return ""
	}
	m.mu.Lock()
	m.discoveryURL = cfg.OIDCDiscoveryURL
	m.resolvedJWKSURL = meta.JWKSURI
	m.mu.Unlock()
	return meta.JWKSURI
}

// jwksKeys returns the JWKS public keys, fetching and caching on first use or
// when refresh is set.
func (m *Method) jwksKeys(ctx context.Context, url string, refresh bool) []crypto.PublicKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !refresh && m.jwksURL == url && m.jwksCache != nil {
		return m.jwksCache
	}
	data, err := m.fetch(ctx, url)
	if err != nil {
		return nil
	}
	keys, err := parseJWKS(data)
	if err != nil {
		return nil
	}
	m.jwksURL = url
	m.jwksCache = keys
	return keys
}

func httpFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwtauth: JWKS fetch: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
