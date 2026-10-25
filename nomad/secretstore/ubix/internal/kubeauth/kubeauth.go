// Package kubeauth implements the Kubernetes auth method (docs/DESIGN.md §3.3,
// roadmap). A pod presents its ServiceAccount token; the method validates it via
// the Kubernetes TokenReview API and, if the reviewed identity matches a
// configured role, issues a vault token carrying that role's policies.
//
// This is the mechanism uBixCore workloads use to obtain a scoped token and then
// read their secrets over the API — no injection layer, no secrets on disk
// (docs/DECISIONS.md D-008).
//
// Validation is behind the [TokenReviewer] seam so the login logic is testable
// without a live cluster; the default reviewer calls the real TokenReview API.
package kubeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// Errors.
var (
	ErrNotConfigured = errors.New("kubeauth: not configured")
	ErrRoleNotFound  = errors.New("kubeauth: role not found")
	ErrInvalidName   = errors.New("kubeauth: invalid role name")
	ErrDenied        = errors.New("kubeauth: permission denied")
)

// Config is the connection to the Kubernetes API used for token review.
type Config struct {
	Host        string `json:"kubernetes_host"`    // apiserver URL
	CACert      string `json:"kubernetes_ca_cert"` // PEM CA bundle (optional)
	ReviewerJWT string `json:"token_reviewer_jwt"` // the vault's own SA token
}

// Role binds a set of ServiceAccounts to the policies granted on login.
type Role struct {
	BoundServiceAccountNames      []string      `json:"bound_service_account_names"`
	BoundServiceAccountNamespaces []string      `json:"bound_service_account_namespaces"`
	Policies                      []string      `json:"policies"`
	TTL                           time.Duration `json:"ttl"`
	TokenMaxTTL                   time.Duration `json:"token_max_ttl"`
}

// ReviewResult is the outcome of validating a ServiceAccount token.
type ReviewResult struct {
	Authenticated  bool
	Namespace      string
	ServiceAccount string
}

// TokenReviewer validates a Kubernetes ServiceAccount token.
type TokenReviewer interface {
	Review(ctx context.Context, jwt string) (*ReviewResult, error)
}

// Storage is the subset of a backend the method needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Method is the Kubernetes auth method.
type Method struct {
	store       Storage
	tokens      *token.Store
	prefix      string
	newReviewer func(Config) (TokenReviewer, error)

	mu       sync.Mutex
	reviewer TokenReviewer
}

// New returns a method storing under prefix (e.g. "auth/kubernetes") that mints
// tokens via tokens and validates logins against the real TokenReview API.
func New(store Storage, tokens *token.Store, prefix string) *Method {
	return &Method{
		store:       store,
		tokens:      tokens,
		prefix:      strings.Trim(prefix, "/"),
		newReviewer: newHTTPReviewer,
	}
}

func (m *Method) configKey() string          { return m.prefix + "/config" }
func (m *Method) roleKey(name string) string { return m.prefix + "/role/" + name }

func validName(name string) bool {
	return name != "" && !strings.Contains(name, "/")
}

// Configure stores the Kubernetes connection config and builds the reviewer.
func (m *Method) Configure(ctx context.Context, cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	reviewer, err := m.newReviewer(cfg)
	if err != nil {
		return fmt.Errorf("kubeauth: build reviewer: %w", err)
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("kubeauth: marshal config: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.configKey(), Value: blob}); err != nil {
		return fmt.Errorf("kubeauth: persist config: %w", err)
	}
	m.reviewer = reviewer
	return nil
}

// Reset forgets the TokenReview client, so the next login rebuilds it from the
// stored config. Called when the vault is sealed.
func (m *Method) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reviewer = nil
}

// ensureReady rebuilds the reviewer from stored config if needed (e.g. restart).
func (m *Method) ensureReady(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reviewer != nil {
		return nil
	}
	entry, err := m.store.Get(ctx, m.configKey())
	if err != nil {
		return fmt.Errorf("kubeauth: read config: %w", err)
	}
	if entry == nil {
		return ErrNotConfigured
	}
	var cfg Config
	if err := json.Unmarshal(entry.Value, &cfg); err != nil {
		return fmt.Errorf("kubeauth: unmarshal config: %w", err)
	}
	reviewer, err := m.newReviewer(cfg)
	if err != nil {
		return fmt.Errorf("kubeauth: build reviewer: %w", err)
	}
	m.reviewer = reviewer
	return nil
}

// WriteRole creates or replaces a role.
func (m *Method) WriteRole(ctx context.Context, name string, role Role) error {
	if !validName(name) {
		return ErrInvalidName
	}
	blob, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("kubeauth: marshal role: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.roleKey(name), Value: blob}); err != nil {
		return fmt.Errorf("kubeauth: persist role: %w", err)
	}
	return nil
}

// ReadRole returns a role, or [ErrRoleNotFound].
func (m *Method) ReadRole(ctx context.Context, name string) (*Role, error) {
	if !validName(name) {
		return nil, ErrInvalidName
	}
	entry, err := m.store.Get(ctx, m.roleKey(name))
	if err != nil {
		return nil, fmt.Errorf("kubeauth: read role: %w", err)
	}
	if entry == nil {
		return nil, ErrRoleNotFound
	}
	var role Role
	if err := json.Unmarshal(entry.Value, &role); err != nil {
		return nil, fmt.Errorf("kubeauth: unmarshal role: %w", err)
	}
	return &role, nil
}

// ListRoles returns all role names.
func (m *Method) ListRoles(ctx context.Context) ([]string, error) {
	names, err := m.store.List(ctx, m.prefix+"/role/")
	if err != nil {
		return nil, fmt.Errorf("kubeauth: list roles: %w", err)
	}
	return names, nil
}

// DeleteRole removes a role.
func (m *Method) DeleteRole(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if err := m.store.Delete(ctx, m.roleKey(name)); err != nil {
		return fmt.Errorf("kubeauth: delete role: %w", err)
	}
	return nil
}

// Login validates a ServiceAccount token against a role and, on success, issues
// a token with the role's policies. It returns [ErrDenied] if the token is
// invalid or the reviewed identity is not bound to the role.
func (m *Method) Login(ctx context.Context, roleName, jwt string) (*token.Token, error) {
	if err := m.ensureReady(ctx); err != nil {
		return nil, err
	}
	role, err := m.ReadRole(ctx, roleName)
	if err != nil {
		return nil, err
	}

	result, err := m.reviewer.Review(ctx, jwt)
	if err != nil {
		return nil, fmt.Errorf("kubeauth: review token: %w", err)
	}
	if !result.Authenticated {
		return nil, ErrDenied
	}
	if !bound(role.BoundServiceAccountNamespaces, result.Namespace) ||
		!bound(role.BoundServiceAccountNames, result.ServiceAccount) {
		return nil, ErrDenied
	}

	subject := result.Namespace + "/" + result.ServiceAccount
	// The role's ttl bounds the token, as with the other auth methods (it was
	// stored and shown but not applied, so every login got the default TTL).
	tok, err := m.tokens.CreateForLogin(ctx, role.Policies, role.TTL, role.TokenMaxTTL, "kubernetes", subject, nil)
	if err != nil {
		return nil, fmt.Errorf("kubeauth: issue token: %w", err)
	}
	return tok, nil
}

// bound reports whether value is permitted by a binding list. A list containing
// "*" permits anything; otherwise the value must be listed explicitly.
func bound(allowed []string, value string) bool {
	for _, a := range allowed {
		if a == "*" || a == value {
			return true
		}
	}
	return false
}
