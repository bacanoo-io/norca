// Package approle implements the AppRole auth method: machine clients present a
// stable, non-secret role_id together with a secret_id (a password-equivalent)
// and receive a vault token carrying the role's policies. It mirrors HashiCorp
// Vault's AppRole shape closely enough for existing tooling.
//
// Storage layout under the mount prefix (e.g. "auth/approle"):
//
//	role/<name>          the role (policies, token TTL, secret_id TTL)
//	roleid/<name>        the role's stable role_id
//	ridx/<role_id>       reverse index: role_id -> role name (for login)
//	sid/<name>/<hash>    a secret_id, stored only as a SHA-256 hash
package approle

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// Errors returned by the method.
var (
	ErrInvalidName   = errors.New("approle: invalid role name")
	ErrRoleNotFound  = errors.New("approle: role not found")
	ErrDenied        = errors.New("approle: invalid role_id or secret_id")
	ErrInvalidConfig = errors.New("approle: role requires at least one policy")
)

// Role is an AppRole: the policies granted on login and the token/secret-id TTLs.
type Role struct {
	Policies    []string      `json:"policies"`
	TokenTTL    time.Duration `json:"token_ttl"`     // 0 uses the token store's default
	TokenMaxTTL time.Duration `json:"token_max_ttl"` // 0 = the system maximum; lowers the token's ceiling
	SecretIDTTL time.Duration `json:"secret_id_ttl"` // 0 means secret_ids do not expire
}

// Storage is the subset of a backend the method needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Method is the AppRole auth method.
type Method struct {
	store  Storage
	tokens *token.Store
	prefix string
}

// New returns a method storing under prefix (e.g. "auth/approle") that mints
// tokens via tokens.
func New(store Storage, tokens *token.Store, prefix string) *Method {
	return &Method{store: store, tokens: tokens, prefix: strings.Trim(prefix, "/")}
}

func (m *Method) roleKey(name string) string   { return m.prefix + "/role/" + name }
func (m *Method) roleIDKey(name string) string { return m.prefix + "/roleid/" + name }
func (m *Method) ridxKey(roleID string) string { return m.prefix + "/ridx/" + roleID }
func (m *Method) sidKey(name, hash string) string {
	return m.prefix + "/sid/" + name + "/" + hash
}

func validName(name string) bool {
	return name != "" && !strings.Contains(name, "/")
}

// randHex returns n random bytes hex-encoded.
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashSecretID(secretID string) string {
	sum := sha256.Sum256([]byte(secretID))
	return hex.EncodeToString(sum[:])
}

// WriteRole creates or replaces a role, assigning a stable role_id the first time.
func (m *Method) WriteRole(ctx context.Context, name string, role Role) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if len(role.Policies) == 0 {
		return ErrInvalidConfig
	}
	blob, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("approle: marshal role: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.roleKey(name), Value: blob}); err != nil {
		return fmt.Errorf("approle: persist role: %w", err)
	}
	// Assign a role_id once and keep it stable across updates.
	if _, err := m.RoleID(ctx, name); errors.Is(err, ErrRoleNotFound) {
		roleID, rerr := randHex(16)
		if rerr != nil {
			return fmt.Errorf("approle: generate role_id: %w", rerr)
		}
		if perr := m.store.Put(ctx, &storage.Entry{Key: m.roleIDKey(name), Value: []byte(roleID)}); perr != nil {
			return fmt.Errorf("approle: persist role_id: %w", perr)
		}
		if perr := m.store.Put(ctx, &storage.Entry{Key: m.ridxKey(roleID), Value: []byte(name)}); perr != nil {
			return fmt.Errorf("approle: persist role_id index: %w", perr)
		}
	} else if err != nil {
		return err
	}
	return nil
}

// ReadRole returns a role, or [ErrRoleNotFound].
func (m *Method) ReadRole(ctx context.Context, name string) (*Role, error) {
	entry, err := m.store.Get(ctx, m.roleKey(name))
	if err != nil {
		return nil, fmt.Errorf("approle: read role: %w", err)
	}
	if entry == nil {
		return nil, ErrRoleNotFound
	}
	var role Role
	if err := json.Unmarshal(entry.Value, &role); err != nil {
		return nil, fmt.Errorf("approle: unmarshal role: %w", err)
	}
	return &role, nil
}

// ListRoles returns the role names.
func (m *Method) ListRoles(ctx context.Context) ([]string, error) {
	names, err := m.store.List(ctx, m.prefix+"/role/")
	if err != nil {
		return nil, fmt.Errorf("approle: list roles: %w", err)
	}
	return names, nil
}

// DeleteRole removes a role, its role_id, and the reverse index.
func (m *Method) DeleteRole(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if roleID, err := m.RoleID(ctx, name); err == nil {
		_ = m.store.Delete(ctx, m.ridxKey(roleID))
	}
	_ = m.store.Delete(ctx, m.roleIDKey(name))
	if err := m.store.Delete(ctx, m.roleKey(name)); err != nil {
		return fmt.Errorf("approle: delete role: %w", err)
	}
	return nil
}

// RoleID returns the role's stable role_id, or [ErrRoleNotFound].
func (m *Method) RoleID(ctx context.Context, name string) (string, error) {
	entry, err := m.store.Get(ctx, m.roleIDKey(name))
	if err != nil {
		return "", fmt.Errorf("approle: read role_id: %w", err)
	}
	if entry == nil {
		return "", ErrRoleNotFound
	}
	return string(entry.Value), nil
}

// secretIDRecord is the stored metadata for a secret_id (never the value itself).
type secretIDRecord struct {
	CreatedUnix int64 `json:"created"`
	ExpiresUnix int64 `json:"expires,omitempty"` // 0 = no expiry
}

// GenerateSecretID mints a new secret_id for a role and returns it once.
func (m *Method) GenerateSecretID(ctx context.Context, name string) (string, error) {
	role, err := m.ReadRole(ctx, name)
	if err != nil {
		return "", err
	}
	secretID, err := randHex(24)
	if err != nil {
		return "", fmt.Errorf("approle: generate secret_id: %w", err)
	}
	now := time.Now().UTC()
	rec := secretIDRecord{CreatedUnix: now.Unix()}
	if role.SecretIDTTL > 0 {
		rec.ExpiresUnix = now.Add(role.SecretIDTTL).Unix()
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("approle: marshal secret_id: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.sidKey(name, hashSecretID(secretID)), Value: blob}); err != nil {
		return "", fmt.Errorf("approle: persist secret_id: %w", err)
	}
	return secretID, nil
}

// Login exchanges a role_id and secret_id for a token carrying the role's
// policies. It returns [ErrDenied] for any mismatch or an expired secret_id,
// without distinguishing the cause.
func (m *Method) Login(ctx context.Context, roleID, secretID string) (*token.Token, error) {
	if roleID == "" || secretID == "" {
		return nil, ErrDenied
	}
	nameEntry, err := m.store.Get(ctx, m.ridxKey(roleID))
	if err != nil {
		return nil, fmt.Errorf("approle: resolve role_id: %w", err)
	}
	if nameEntry == nil {
		return nil, ErrDenied
	}
	name := string(nameEntry.Value)

	role, err := m.ReadRole(ctx, name)
	if err != nil {
		return nil, ErrDenied
	}

	sidKey := m.sidKey(name, hashSecretID(secretID))
	sidEntry, err := m.store.Get(ctx, sidKey)
	if err != nil {
		return nil, fmt.Errorf("approle: read secret_id: %w", err)
	}
	if sidEntry == nil {
		return nil, ErrDenied
	}
	var rec secretIDRecord
	if err := json.Unmarshal(sidEntry.Value, &rec); err != nil {
		return nil, fmt.Errorf("approle: unmarshal secret_id: %w", err)
	}
	if rec.ExpiresUnix != 0 && time.Now().Unix() >= rec.ExpiresUnix {
		_ = m.store.Delete(ctx, sidKey) // best-effort cleanup of the expired secret_id
		return nil, ErrDenied
	}

	if role.TokenTTL > 0 {
		return m.tokens.CreateForLogin(ctx, role.Policies, role.TokenTTL, role.TokenMaxTTL, "approle", name, nil)
	}
	return m.tokens.CreateForLogin(ctx, role.Policies, 0, role.TokenMaxTTL, "approle", name, nil)
}
