// Package database implements the dynamic database secrets engine
// (docs/DESIGN.md §3.6): it generates short-lived database credentials on demand
// and revokes them when their lease expires. This is the defining dynamic-secret
// capability — on-demand generation with lease-bound revocation.
//
// The engine is database-agnostic. All database-specific behavior lives behind
// the [Plugin] interface (the DatabasePlugin seam from docs/DESIGN.md §7):
// creating and revoking users. MariaDB is the reference plugin (docs/DECISIONS.md
// D-006); this package is unit-tested against a mock plugin, so the lease
// lifecycle is exercised without a real database.
package database

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// Errors.
var (
	ErrNotConfigured = errors.New("database: engine not configured")
	ErrRoleNotFound  = errors.New("database: role not found")
	ErrLeaseNotFound = errors.New("database: lease not found")
	ErrInvalidName   = errors.New("database: invalid name")
)

// CreateUserRequest is what the engine asks a [Plugin] to create. The plugin
// substitutes {{username}}, {{password}}, and {{expiration}} into the creation
// statements and executes them.
type CreateUserRequest struct {
	Username           string
	Password           string
	Expiration         time.Time
	CreationStatements []string
}

// Plugin is the database-specific seam: everything the engine needs that depends
// on the target database. Implementations must be safe for concurrent use.
type Plugin interface {
	// Initialize opens and verifies a connection using the given URL.
	Initialize(ctx context.Context, connectionURL string) error
	// CreateUser creates a database user per the request.
	CreateUser(ctx context.Context, req CreateUserRequest) error
	// RevokeUser drops the named database user.
	RevokeUser(ctx context.Context, username string) error
	// Close releases the underlying connection.
	Close() error
}

// Role defines how credentials are minted: the SQL run to create a user and the
// lease duration.
type Role struct {
	CreationStatements []string      `json:"creation_statements"`
	DefaultTTL         time.Duration `json:"default_ttl"`
}

// Credential is a freshly issued dynamic credential and its lease.
type Credential struct {
	LeaseID  string        `json:"lease_id"`
	Username string        `json:"username"`
	Password string        `json:"password"`
	TTL      time.Duration `json:"ttl"`
}

// lease is the persisted record used to revoke a credential later.
type lease struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Username   string    `json:"username"`
	Expiration time.Time `json:"expiration"`
	CreatedBy  string    `json:"created_by,omitempty"` // token that requested it (for cascading revocation)
}

// LeaseInfo is the non-secret view of a lease.
type LeaseInfo struct {
	LeaseID  string
	Role     string
	TTL      time.Duration // remaining time until expiration
	Username string
}

// Storage is the subset of a backend the engine needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Engine is the dynamic database secrets engine.
type Engine struct {
	store   Storage
	prefix  string
	plugin  Plugin
	now     func() time.Time
	genUser func(role string) string
	genPass func() (string, error)

	mu    sync.Mutex // guards plugin initialization
	ready bool       // plugin has been initialized against the stored config
}

// New returns an engine storing under prefix (e.g. "database") and using the
// given plugin for database operations.
func New(store Storage, prefix string, plugin Plugin) *Engine {
	return &Engine{
		store:   store,
		prefix:  strings.Trim(prefix, "/"),
		plugin:  plugin,
		now:     func() time.Time { return time.Now().UTC() },
		genUser: defaultUsername,
		genPass: defaultPassword,
	}
}

func (e *Engine) configKey() string          { return e.prefix + "/config" }
func (e *Engine) roleKey(name string) string { return e.prefix + "/role/" + name }
func (e *Engine) leaseKey(id string) string  { return e.prefix + "/lease/" + id }

// Configure records the connection URL and initializes the plugin against it.
func (e *Engine) Configure(ctx context.Context, connectionURL string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.plugin.Initialize(ctx, connectionURL); err != nil {
		return fmt.Errorf("database: initialize plugin: %w", err)
	}
	blob, err := json.Marshal(map[string]string{"connection_url": connectionURL})
	if err != nil {
		return fmt.Errorf("database: marshal config: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.configKey(), Value: blob}); err != nil {
		return fmt.Errorf("database: persist config: %w", err)
	}
	e.ready = true
	return nil
}

// Reset closes the plugin's connection and forgets it, so the next use
// re-reads the stored config. Called when the vault is sealed.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ready {
		_ = e.plugin.Close()
		e.ready = false
	}
}

// ensureReady initializes the plugin from the stored connection URL if it has
// not been initialized in this process yet (e.g. after a restart). It returns
// [ErrNotConfigured] if the engine was never configured.
func (e *Engine) ensureReady(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ready {
		return nil
	}
	entry, err := e.store.Get(ctx, e.configKey())
	if err != nil {
		return fmt.Errorf("database: read config: %w", err)
	}
	if entry == nil {
		return ErrNotConfigured
	}
	var cfg struct {
		ConnectionURL string `json:"connection_url"`
	}
	if err := json.Unmarshal(entry.Value, &cfg); err != nil {
		return fmt.Errorf("database: unmarshal config: %w", err)
	}
	if err := e.plugin.Initialize(ctx, cfg.ConnectionURL); err != nil {
		return fmt.Errorf("database: initialize plugin: %w", err)
	}
	e.ready = true
	return nil
}

// Configured reports whether the engine has been configured.
func (e *Engine) Configured(ctx context.Context) (bool, error) {
	entry, err := e.store.Get(ctx, e.configKey())
	if err != nil {
		return false, fmt.Errorf("database: read config: %w", err)
	}
	return entry != nil, nil
}

// WriteRole creates or replaces a role.
func (e *Engine) WriteRole(ctx context.Context, name string, role Role) error {
	if !validName(name) {
		return ErrInvalidName
	}
	blob, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("database: marshal role: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.roleKey(name), Value: blob}); err != nil {
		return fmt.Errorf("database: persist role: %w", err)
	}
	return nil
}

// ReadRole returns a role, or [ErrRoleNotFound].
func (e *Engine) ReadRole(ctx context.Context, name string) (*Role, error) {
	if !validName(name) {
		return nil, ErrInvalidName
	}
	entry, err := e.store.Get(ctx, e.roleKey(name))
	if err != nil {
		return nil, fmt.Errorf("database: read role: %w", err)
	}
	if entry == nil {
		return nil, ErrRoleNotFound
	}
	var role Role
	if err := json.Unmarshal(entry.Value, &role); err != nil {
		return nil, fmt.Errorf("database: unmarshal role: %w", err)
	}
	return &role, nil
}

// ListRoles returns all role names.
func (e *Engine) ListRoles(ctx context.Context) ([]string, error) {
	names, err := e.store.List(ctx, e.prefix+"/role/")
	if err != nil {
		return nil, fmt.Errorf("database: list roles: %w", err)
	}
	return names, nil
}

// DeleteRole removes a role.
func (e *Engine) DeleteRole(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if err := e.store.Delete(ctx, e.roleKey(name)); err != nil {
		return fmt.Errorf("database: delete role: %w", err)
	}
	return nil
}

// GenerateCredentials issues a fresh short-lived credential for a role: it
// generates a username and password, has the plugin create the database user,
// records a lease attributed to createdBy (the requesting token, "" if none),
// and returns the credential.
func (e *Engine) GenerateCredentials(ctx context.Context, roleName, createdBy string) (*Credential, error) {
	if err := e.ensureReady(ctx); err != nil {
		return nil, err
	}

	role, err := e.ReadRole(ctx, roleName)
	if err != nil {
		return nil, err
	}

	username := e.genUser(roleName)
	password, err := e.genPass()
	if err != nil {
		return nil, err
	}
	expiration := e.now().Add(role.DefaultTTL)

	if err := e.plugin.CreateUser(ctx, CreateUserRequest{
		Username:           username,
		Password:           password,
		Expiration:         expiration,
		CreationStatements: role.CreationStatements,
	}); err != nil {
		return nil, fmt.Errorf("database: create user: %w", err)
	}

	id, err := randomID()
	if err != nil {
		return nil, err
	}
	if err := e.saveLease(ctx, lease{ID: id, Role: roleName, Username: username, Expiration: expiration, CreatedBy: createdBy}); err != nil {
		// Best-effort cleanup: the user was created but the lease could not be
		// recorded, so drop the user rather than leak an untracked credential.
		_ = e.plugin.RevokeUser(ctx, username)
		return nil, err
	}

	return &Credential{LeaseID: id, Username: username, Password: password, TTL: role.DefaultTTL}, nil
}

// Lookup returns the non-secret information for a lease.
func (e *Engine) Lookup(ctx context.Context, leaseID string) (*LeaseInfo, error) {
	l, err := e.loadLease(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	ttl := time.Until(l.Expiration)
	if ttl < 0 {
		ttl = 0
	}
	return &LeaseInfo{LeaseID: l.ID, Role: l.Role, TTL: ttl, Username: l.Username}, nil
}

// Renew extends a lease's expiration by ttl from now (or the role's default TTL
// if ttl <= 0) and returns the updated lease info. This delays revocation of the
// underlying database user by the expiry sweeper.
func (e *Engine) Renew(ctx context.Context, leaseID string, ttl time.Duration) (*LeaseInfo, error) {
	l, err := e.loadLease(ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		if role, rerr := e.ReadRole(ctx, l.Role); rerr == nil {
			ttl = role.DefaultTTL
		}
	}
	l.Expiration = e.now().Add(ttl)
	if err := e.saveLease(ctx, *l); err != nil {
		return nil, err
	}
	return &LeaseInfo{LeaseID: l.ID, Role: l.Role, TTL: ttl, Username: l.Username}, nil
}

// RevokeByToken revokes every lease created by the given token and returns the
// count. This is the cascade applied when a token is revoked, so its dynamic
// credentials do not outlive it.
func (e *Engine) RevokeByToken(ctx context.Context, tokenID string) (int, error) {
	if tokenID == "" {
		return 0, nil
	}
	ids, err := e.store.List(ctx, e.prefix+"/lease/")
	if err != nil {
		return 0, fmt.Errorf("database: list leases: %w", err)
	}
	revoked := 0
	for _, id := range ids {
		l, err := e.loadLease(ctx, id)
		if err != nil {
			continue
		}
		if l.CreatedBy != tokenID {
			continue
		}
		if err := e.Revoke(ctx, id); err != nil {
			return revoked, err
		}
		revoked++
	}
	return revoked, nil
}

// Revoke drops the credential for a lease and removes the lease.
func (e *Engine) Revoke(ctx context.Context, leaseID string) error {
	if err := e.ensureReady(ctx); err != nil {
		return err
	}
	l, err := e.loadLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if err := e.plugin.RevokeUser(ctx, l.Username); err != nil {
		return fmt.Errorf("database: revoke user: %w", err)
	}
	if err := e.store.Delete(ctx, e.leaseKey(leaseID)); err != nil {
		return fmt.Errorf("database: delete lease: %w", err)
	}
	return nil
}

// RevokeExpired revokes every lease whose expiration has passed and returns the
// number revoked. A server calls this periodically to enforce TTLs.
func (e *Engine) RevokeExpired(ctx context.Context) (int, error) {
	ids, err := e.store.List(ctx, e.prefix+"/lease/")
	if err != nil {
		return 0, fmt.Errorf("database: list leases: %w", err)
	}
	now := e.now()
	revoked := 0
	for _, id := range ids {
		l, err := e.loadLease(ctx, id)
		if err != nil {
			continue
		}
		if now.Before(l.Expiration) {
			continue
		}
		if err := e.Revoke(ctx, id); err != nil {
			return revoked, err
		}
		revoked++
	}
	return revoked, nil
}

// CountLeasesUnder returns the number of active leases whose issuing path
// (database/creds/<role>) has pathPrefix as a prefix. It backs lease-count
// quota enforcement; an empty prefix counts every lease in this engine.
func (e *Engine) CountLeasesUnder(ctx context.Context, pathPrefix string) (int, error) {
	ids, err := e.store.List(ctx, e.prefix+"/lease/")
	if err != nil {
		return 0, fmt.Errorf("database: list leases: %w", err)
	}
	n := 0
	for _, id := range ids {
		l, err := e.loadLease(ctx, id)
		if err != nil {
			continue
		}
		if strings.HasPrefix(e.prefix+"/creds/"+l.Role, pathPrefix) {
			n++
		}
	}
	return n, nil
}

func (e *Engine) saveLease(ctx context.Context, l lease) error {
	blob, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("database: marshal lease: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.leaseKey(l.ID), Value: blob}); err != nil {
		return fmt.Errorf("database: persist lease: %w", err)
	}
	return nil
}

func (e *Engine) loadLease(ctx context.Context, id string) (*lease, error) {
	entry, err := e.store.Get(ctx, e.leaseKey(id))
	if err != nil {
		return nil, fmt.Errorf("database: read lease: %w", err)
	}
	if entry == nil {
		return nil, ErrLeaseNotFound
	}
	var l lease
	if err := json.Unmarshal(entry.Value, &l); err != nil {
		return nil, fmt.Errorf("database: unmarshal lease: %w", err)
	}
	return &l, nil
}

// validName restricts role names to a single, path-safe segment.
func validName(name string) bool {
	return name != "" && !strings.Contains(name, "/") && storage.ValidateKey("database/role/"+name) == nil
}

func defaultUsername(role string) string {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	if len(role) > 8 {
		role = role[:8]
	}
	return fmt.Sprintf("uv_%s_%s", role, hex.EncodeToString(suffix))
}

func defaultPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("database: generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("database: generate lease id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
