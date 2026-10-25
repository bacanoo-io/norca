// Package ldapauth implements the LDAP / Active Directory auth method: a client
// logs in with a directory username and password, the vault verifies them by
// binding to the directory, reads the user's group memberships, and issues a
// token whose policies come from a group→policy map and/or the identity layer's
// external groups (the asserted LDAP groups flow through the token.Aliaser seam,
// just like an OIDC groups claim).
//
// All LDAP protocol handling lives behind the [Connector] seam (see
// ldap_conn.go, which adapts github.com/go-ldap/ldap/v3, ADR D-018); the method
// logic here is stdlib and unit-tested with a fake connector.
package ldapauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// Errors.
var (
	ErrDenied        = errors.New("ldapauth: authentication failed")
	ErrNotConfigured = errors.New("ldapauth: not configured")
	ErrInvalidName   = errors.New("ldapauth: invalid group name")
	ErrInvalidConfig = errors.New("ldapauth: config needs a url and user_dn")
	// ErrInvalidCertificate is returned when certificate is not a PEM bundle of
	// one or more CA certificates.
	ErrInvalidCertificate = errors.New("ldapauth: certificate must be a PEM bundle of one or more CA certificates")
	// ErrCertificateWithInsecureTLS is returned when both are set: with
	// verification off the CA would never be consulted, which reads as secure
	// and is not.
	ErrCertificateWithInsecureTLS = errors.New("ldapauth: certificate has no effect with insecure_tls; drop insecure_tls to verify against it")
)

// Config holds the directory connection and search settings.
type Config struct {
	URL         string `json:"url"`          // ldap://host:389 or ldaps://host:636
	StartTLS    bool   `json:"starttls"`     // upgrade a plain connection to TLS
	InsecureTLS bool   `json:"insecure_tls"` // skip certificate verification (dev only)
	// Certificate is a PEM bundle of the CA certificates that sign the
	// directory's TLS certificate (e.g. FreeIPA's Dogtag CA, an AD enterprise
	// CA). When set, only these CAs are trusted for this connection — the
	// system roots are not consulted — as with Vault's LDAP "certificate".
	Certificate  string        `json:"certificate,omitempty"`
	BindDN       string        `json:"bind_dn"`       // service account used to search; empty = anonymous
	BindPassword string        `json:"bind_password"` // service-account password
	UserDN       string        `json:"user_dn"`       // base DN to search for users
	UserAttr     string        `json:"user_attr"`     // attribute matching the login name (default "cn")
	GroupDN      string        `json:"group_dn"`      // base DN to search for groups
	GroupAttr    string        `json:"group_attr"`    // attribute holding the group name (default "cn")
	GroupFilter  string        `json:"group_filter"`  // group search filter; {{.UserDN}}/{{.Username}} substituted (default "(member={{.UserDN}})")
	TokenTTL     time.Duration `json:"token_ttl"`     // 0 = default TTL
	TokenMaxTTL  time.Duration `json:"token_max_ttl"` // 0 = the system maximum; lowers the token's ceiling
}

// Connector performs the actual LDAP conversation: bind as the service account,
// find the user, verify the password by binding as the user, and read the user's
// groups. It is the single seam that touches the LDAP library, so the method is
// testable without a directory. A wrong username/password must return
// [ErrDenied]; connection failures return a wrapped error.
type Connector interface {
	Authenticate(ctx context.Context, cfg *Config, username, password string) (groups []string, err error)
}

// Storage is the subset of a backend the method needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Method is the LDAP auth method.
type Method struct {
	store  Storage
	tokens *token.Store
	prefix string
	conn   Connector
}

// New returns a method storing under prefix (e.g. "auth/ldap") that talks to a
// real directory.
func New(store Storage, tokens *token.Store, prefix string) *Method {
	return NewWithConnector(store, tokens, prefix, realConnector{})
}

// NewWithConnector is New with an injectable connector, for tests.
func NewWithConnector(store Storage, tokens *token.Store, prefix string, conn Connector) *Method {
	return &Method{store: store, tokens: tokens, prefix: strings.Trim(prefix, "/"), conn: conn}
}

func (m *Method) configKey() string           { return m.prefix + "/config" }
func (m *Method) groupKey(name string) string { return m.prefix + "/groups/" + name }

func validName(name string) bool { return name != "" && !strings.Contains(name, "/") }

// Configure stores the directory config.
func (m *Method) Configure(ctx context.Context, cfg Config) error {
	if cfg.URL == "" || cfg.UserDN == "" {
		return ErrInvalidConfig
	}
	if cfg.Certificate != "" {
		if cfg.InsecureTLS {
			return ErrCertificateWithInsecureTLS
		}
		if _, err := certPool(cfg.Certificate); err != nil {
			return err
		}
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("ldapauth: marshal config: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.configKey(), Value: blob}); err != nil {
		return fmt.Errorf("ldapauth: persist config: %w", err)
	}
	return nil
}

// ReadConfig returns the stored config, or [ErrNotConfigured].
func (m *Method) ReadConfig(ctx context.Context) (*Config, error) {
	entry, err := m.store.Get(ctx, m.configKey())
	if err != nil {
		return nil, fmt.Errorf("ldapauth: read config: %w", err)
	}
	if entry == nil {
		return nil, ErrNotConfigured
	}
	var cfg Config
	if err := json.Unmarshal(entry.Value, &cfg); err != nil {
		return nil, fmt.Errorf("ldapauth: unmarshal config: %w", err)
	}
	return &cfg, nil
}

// WriteGroup maps an LDAP group name to a set of policies.
func (m *Method) WriteGroup(ctx context.Context, name string, policies []string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	blob, err := json.Marshal(policies)
	if err != nil {
		return fmt.Errorf("ldapauth: marshal group: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: m.groupKey(name), Value: blob}); err != nil {
		return fmt.Errorf("ldapauth: persist group: %w", err)
	}
	return nil
}

// ReadGroup returns the policies mapped to an LDAP group (nil if unmapped).
func (m *Method) ReadGroup(ctx context.Context, name string) ([]string, error) {
	entry, err := m.store.Get(ctx, m.groupKey(name))
	if err != nil {
		return nil, fmt.Errorf("ldapauth: read group: %w", err)
	}
	if entry == nil {
		return nil, nil
	}
	var policies []string
	if err := json.Unmarshal(entry.Value, &policies); err != nil {
		return nil, fmt.Errorf("ldapauth: unmarshal group: %w", err)
	}
	return policies, nil
}

// ListGroups returns the mapped group names.
func (m *Method) ListGroups(ctx context.Context) ([]string, error) {
	names, err := m.store.List(ctx, m.prefix+"/groups/")
	if err != nil {
		return nil, fmt.Errorf("ldapauth: list groups: %w", err)
	}
	return names, nil
}

// DeleteGroup removes a group→policy mapping.
func (m *Method) DeleteGroup(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if err := m.store.Delete(ctx, m.groupKey(name)); err != nil {
		return fmt.Errorf("ldapauth: delete group: %w", err)
	}
	return nil
}

// Login verifies username/password against the directory and issues a token. Its
// policies are the union of those mapped to the user's LDAP groups; the same
// groups are asserted to the identity layer (external groups). Bad credentials
// return [ErrDenied].
func (m *Method) Login(ctx context.Context, username, password string) (*token.Token, error) {
	cfg, err := m.ReadConfig(ctx)
	if err != nil {
		return nil, err
	}
	// An empty password would trigger an unauthenticated LDAP bind that "succeeds"
	// without proving anything — reject it before we ever reach the directory.
	if username == "" || password == "" {
		return nil, ErrDenied
	}

	groups, err := m.conn.Authenticate(ctx, cfg, username, password)
	if err != nil {
		return nil, err
	}

	policySet := map[string]bool{}
	for _, g := range groups {
		mapped, err := m.ReadGroup(ctx, g)
		if err != nil {
			return nil, err
		}
		for _, p := range mapped {
			policySet[p] = true
		}
	}
	policies := make([]string, 0, len(policySet))
	for p := range policySet {
		policies = append(policies, p)
	}
	sort.Strings(policies)

	if cfg.TokenTTL > 0 {
		return m.tokens.CreateForLogin(ctx, policies, cfg.TokenTTL, cfg.TokenMaxTTL, "ldap", username, groups)
	}
	return m.tokens.CreateForLogin(ctx, policies, 0, cfg.TokenMaxTTL, "ldap", username, groups)
}
