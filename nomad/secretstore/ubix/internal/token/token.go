// Package token implements token storage: the credentials clients present to
// authenticate (docs/DESIGN.md §3.3). A token carries the set of ACL policies
// that authorize its requests; the special "root" policy grants everything.
//
// Tokens are stored in the barrier (encrypted at rest) but indexed by the
// SHA-256 of the token value, never by the value itself. The barrier encrypts
// values but not keys, so keying by the raw token would leak it in on-disk key
// names. Because token values are high-entropy random strings, a plain hash
// index is sufficient (there is nothing to brute-force).
package token

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// RootPolicy is the policy name that grants unrestricted access.
const RootPolicy = "root"

// displayPrefix makes tokens recognizable; it is part of the token value.
const displayPrefix = "uv."

// storePrefix is the storage namespace for token records.
const storePrefix = "sys/token/"

// childPrefix indexes parent -> child: sys/token-child/<parent hash>/<child hash>.
// Keys hold only hashes, like the token records themselves (see storeKey).
const childPrefix = "sys/token-child/"

// idBytes is the amount of entropy in a token value.
const idBytes = 24

// ErrTokenNotFound is returned when a token id has no record.
// Token errors.
var (
	ErrTokenNotFound = errors.New("token: not found")
	ErrTokenExpired  = errors.New("token: expired")
)

// DefaultTTL is applied to tokens created without an explicit TTL. Root tokens
// never expire.
const DefaultTTL = 32 * 24 * time.Hour // 32 days, matching Vault's default

// DefaultMaxTTL is the system maximum token lifetime (see [Store.SetMaxTTL]):
// how far renewal may extend a token whose own TTL is shorter. It matches
// Vault's default max_lease_ttl.
const DefaultMaxTTL = 32 * 24 * time.Hour

// Token is an authentication credential with attached policies.
type Token struct {
	ID          string    `json:"id"`
	Policies    []string  `json:"policies"`
	EntityID    string    `json:"entity_id,omitempty"` // identity entity this token belongs to; empty if none
	CreatedTime time.Time `json:"created_time"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"` // zero means never expires
	// MaxExpiresAt is the latest ExpiresAt renewal may reach, fixed at creation
	// (see [Store.MaxExpiry], ADR D-022). Zero on a non-expiring token, and on tokens
	// stored before it existed, whose ceiling is derived on first renewal.
	MaxExpiresAt time.Time `json:"max_expires_at,omitempty"`
	// ParentHash is the storage hash of the token that created this one (see
	// [Store.CreateChild]); empty for an orphan — root tokens, auth-method
	// logins, and tokens created by root or a sudo holder. Revoking a parent
	// revokes its children (ADR D-022). Never the parent's raw ID.
	ParentHash string `json:"parent,omitempty"`
}

// IsOrphan reports whether the token has no parent.
func (t *Token) IsOrphan() bool { return t.ParentHash == "" }

// IsRoot reports whether the token carries the root policy.
func (t *Token) IsRoot() bool {
	for _, p := range t.Policies {
		if p == RootPolicy {
			return true
		}
	}
	return false
}

// expired reports whether the token has passed its expiration.
func (t *Token) expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && now.After(t.ExpiresAt)
}

// Storage is the subset of a backend the token store needs. *barrier.Barrier
// and the raw backends satisfy it.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Aliaser resolves an auth-method login to an identity entity, creating one on
// first sight (unless auto-creation is disabled). It is the seam through which
// the identity layer stamps an entity onto tokens minted by a login, without the
// token store depending on the identity package. A nil aliaser (the default)
// means no identity: tokens are minted with an empty EntityID.
//
// groups carries any group memberships the auth method asserted for this login
// (e.g. an OIDC groups claim); the identity layer records them so external
// groups can match. It may be nil.
type Aliaser interface {
	ResolveAlias(ctx context.Context, mountType, name string, groups []string) (entityID string, err error)
}

// Store persists and retrieves tokens.
type Store struct {
	store   Storage
	now     func() time.Time
	aliaser Aliaser
	maxTTL  time.Duration
}

// NewStore returns a token store over s.
func NewStore(s Storage) *Store {
	return &Store{store: s, now: func() time.Time { return time.Now().UTC() }, maxTTL: DefaultMaxTTL}
}

// SetMaxTTL sets the system maximum token lifetime used for tokens created from
// now on (and for older tokens on their first renewal). A token's ceiling is
// the later of its own creation TTL and this maximum, so an operator can still
// issue a long-lived token deliberately; what the maximum bounds is how far a
// shorter-lived token can renew itself. d <= 0 restores [DefaultMaxTTL].
func (st *Store) SetMaxTTL(d time.Duration) {
	if d <= 0 {
		d = DefaultMaxTTL
	}
	st.maxTTL = d
}

// MaxExpiry is the latest time t may be renewed to: its stored ceiling, or for a
// token stored before ceilings existed, the later of its current expiry and its
// creation time plus the system maximum (so no existing token gets shorter).
// It is zero for a non-expiring token.
func (st *Store) MaxExpiry(t *Token) time.Time {
	if t.ExpiresAt.IsZero() {
		return time.Time{}
	}
	if !t.MaxExpiresAt.IsZero() {
		return t.MaxExpiresAt
	}
	return later(t.ExpiresAt, t.CreatedTime.Add(st.maxTTL))
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// SetAliaser installs the identity resolver used by the alias-aware create
// methods. It is wired once at startup; passing nil disables identity binding.
func (st *Store) SetAliaser(a Aliaser) { st.aliaser = a }

// CreateRoot creates a non-expiring token with the root policy.
func (st *Store) CreateRoot(ctx context.Context) (*Token, error) {
	return st.create(ctx, []string{RootPolicy}, time.Time{})
}

// Create issues a token with the given policies and the default TTL.
func (st *Store) Create(ctx context.Context, policies []string) (*Token, error) {
	return st.create(ctx, policies, st.now().Add(DefaultTTL))
}

// CreateWithTTL issues a token that expires after ttl. A ttl <= 0 means the
// token never expires.
func (st *Store) CreateWithTTL(ctx context.Context, policies []string, ttl time.Duration) (*Token, error) {
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = st.now().Add(ttl)
	}
	return st.create(ctx, policies, expiresAt)
}

// CreateChild issues a child of parent: it expires after ttl (the default TTL
// when ttl <= 0), can never be renewed past the parent's ceiling
// ([Store.MaxExpiry]), and is revoked when the parent is revoked or expires
// ([Store.RevokeTree]). It is how a token without root or sudo creates tokens.
func (st *Store) CreateChild(ctx context.Context, parent *Token, policies []string, ttl time.Duration) (*Token, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	parentHash := hashID(parent.ID)
	t, err := st.createWithEntityBounded(ctx, policies, st.now().Add(ttl), "", st.MaxExpiry(parent), parentHash)
	if err != nil {
		return nil, err
	}
	// The index entry is what lets a revoke find the child. Written after the
	// child record; if it fails the child is removed rather than left orphaned.
	if err := st.store.Put(ctx, &storage.Entry{Key: childKey(parentHash, hashID(t.ID)), Value: []byte("1")}); err != nil {
		_ = st.store.Delete(ctx, storeKey(t.ID))
		return nil, fmt.Errorf("token: index child: %w", err)
	}
	return t, nil
}

// CreateWithAlias issues a token with the default TTL, binding it to the
// identity entity that (mountType, name) resolves to. groups carries any
// auth-method-asserted group memberships (may be nil). It is the alias-aware
// counterpart of Create, called by the auth methods at login.
func (st *Store) CreateWithAlias(ctx context.Context, policies []string, mountType, name string, groups []string) (*Token, error) {
	return st.createAlias(ctx, policies, st.now().Add(DefaultTTL), mountType, name, groups)
}

// CreateWithTTLAndAlias is CreateWithTTL bound to an identity entity. A ttl <= 0
// means the token never expires.
func (st *Store) CreateWithTTLAndAlias(ctx context.Context, policies []string, ttl time.Duration, mountType, name string, groups []string) (*Token, error) {
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = st.now().Add(ttl)
	}
	return st.createAlias(ctx, policies, expiresAt, mountType, name, groups)
}

// CreateForLogin is what the auth methods call at login: a token bound to the
// identity entity (mountType, name) resolves to, lasting ttl (the default TTL
// when ttl <= 0). A role's maxTTL (> 0) lowers the token's ceiling to now+maxTTL:
// its expiry and any renewal stop there. It never raises the ceiling — the
// system maximum still applies (see [Store.MaxExpiry]).
func (st *Store) CreateForLogin(ctx context.Context, policies []string, ttl, maxTTL time.Duration, mountType, name string, groups []string) (*Token, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	var bound time.Time
	if maxTTL > 0 {
		bound = st.now().Add(maxTTL)
	}
	entityID, err := st.resolveEntity(ctx, mountType, name, groups)
	if err != nil {
		return nil, err
	}
	return st.createWithEntityBounded(ctx, policies, st.now().Add(ttl), entityID, bound, "") // logins are orphans
}

// resolveEntity returns the identity entity for a login, or "" without an
// aliaser or a name.
func (st *Store) resolveEntity(ctx context.Context, mountType, name string, groups []string) (string, error) {
	if st.aliaser == nil || name == "" {
		return "", nil
	}
	id, err := st.aliaser.ResolveAlias(ctx, mountType, name, groups)
	if err != nil {
		return "", fmt.Errorf("token: resolve identity: %w", err)
	}
	return id, nil
}

// createAlias resolves the alias to an entity (if an aliaser is installed and a
// name is supplied) and mints a token bound to it.
func (st *Store) createAlias(ctx context.Context, policies []string, expiresAt time.Time, mountType, name string, groups []string) (*Token, error) {
	entityID := ""
	if st.aliaser != nil && name != "" {
		id, err := st.aliaser.ResolveAlias(ctx, mountType, name, groups)
		if err != nil {
			return nil, fmt.Errorf("token: resolve identity: %w", err)
		}
		entityID = id
	}
	return st.createWithEntity(ctx, policies, expiresAt, entityID)
}

func (st *Store) create(ctx context.Context, policies []string, expiresAt time.Time) (*Token, error) {
	return st.createWithEntity(ctx, policies, expiresAt, "")
}

func (st *Store) createWithEntity(ctx context.Context, policies []string, expiresAt time.Time, entityID string) (*Token, error) {
	return st.createWithEntityBounded(ctx, policies, expiresAt, entityID, time.Time{}, "")
}

// createWithEntityBounded mints and stores a token. An expiring token gets its
// renewal ceiling here: the later of its own expiry and now plus the system
// maximum, then clamped (with the expiry) to bound when bound is non-zero.
func (st *Store) createWithEntityBounded(ctx context.Context, policies []string, expiresAt time.Time, entityID string, bound time.Time, parentHash string) (*Token, error) {
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := st.now()
	var maxExpiresAt time.Time
	if !expiresAt.IsZero() {
		maxExpiresAt = later(expiresAt, now.Add(st.maxTTL))
		if !bound.IsZero() {
			if expiresAt.After(bound) {
				expiresAt = bound
			}
			if maxExpiresAt.After(bound) {
				maxExpiresAt = bound
			}
		}
	}
	t := &Token{ID: id, Policies: policies, EntityID: entityID, CreatedTime: now, ExpiresAt: expiresAt, MaxExpiresAt: maxExpiresAt, ParentHash: parentHash}
	if err := st.save(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// Lookup returns the token with the given id. It returns [ErrTokenNotFound] if
// absent and [ErrTokenExpired] if it has expired (deleting the expired record).
func (st *Store) Lookup(ctx context.Context, id string) (*Token, error) {
	entry, err := st.store.Get(ctx, storeKey(id))
	if err != nil {
		return nil, fmt.Errorf("token: lookup: %w", err)
	}
	if entry == nil {
		return nil, ErrTokenNotFound
	}
	var t Token
	if err := json.Unmarshal(entry.Value, &t); err != nil {
		return nil, fmt.Errorf("token: unmarshal: %w", err)
	}
	if t.expired(st.now()) {
		// Refused at once, but not deleted here: removal goes through the sweeper
		// (SweepExpired), which also revokes the token's children and what it owned.
		return nil, ErrTokenExpired
	}
	return &t, nil
}

// Renew sets a token's expiration to ttl from now (the default TTL if ttl <= 0),
// but never past its renewal ceiling ([Store.MaxExpiry]); a token stored before
// ceilings existed has its derived ceiling recorded here, so it stays fixed.
// Root and other non-expiring tokens are returned unchanged. It returns
// [ErrTokenNotFound]/[ErrTokenExpired] like Lookup.
func (st *Store) Renew(ctx context.Context, id string, ttl time.Duration) (*Token, error) {
	t, err := st.Lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.ExpiresAt.IsZero() {
		return t, nil // never-expiring token; nothing to extend
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	ceiling := st.MaxExpiry(t)
	t.MaxExpiresAt = ceiling
	t.ExpiresAt = st.now().Add(ttl)
	if t.ExpiresAt.After(ceiling) {
		t.ExpiresAt = ceiling
	}
	if err := st.save(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// SweepExpired deletes up to limit expired token records (limit <= 0 means no
// limit) and returns how many it removed. For each, cleanup is called with the
// token's ID first — to destroy what the token owned (its cubbyhole, its
// dynamic-database leases), as revoke-self does — and a token whose cleanup
// fails is left in place to be retried on the next sweep.
//
// Without this an expired token is only deleted when something happens to look
// it up, so a client that logs in on every request leaves every token behind:
// storage grows without bound, one row per login.
func (st *Store) SweepExpired(ctx context.Context, limit int, cleanup func(ctx context.Context, id string) error) (int, error) {
	names, err := st.store.List(ctx, storePrefix)
	if err != nil {
		return 0, fmt.Errorf("token: list: %w", err)
	}
	now := st.now()
	removed := 0
	for _, name := range names {
		if limit > 0 && removed >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		key := storePrefix + name
		entry, err := st.store.Get(ctx, key)
		if err != nil {
			return removed, fmt.Errorf("token: read: %w", err)
		}
		if entry == nil {
			continue // revoked meanwhile
		}
		var t Token
		if err := json.Unmarshal(entry.Value, &t); err != nil {
			continue // not ours to judge; leave it
		}
		if !t.expired(now) {
			continue
		}
		n, err := st.revokeTree(ctx, t.ID, name, cleanup, 0)
		removed += n
		if err != nil {
			continue // retried next sweep
		}
	}
	return removed, nil
}

func (st *Store) save(ctx context.Context, t *Token) error {
	blob, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("token: marshal: %w", err)
	}
	if err := st.store.Put(ctx, &storage.Entry{Key: storeKey(t.ID), Value: blob}); err != nil {
		return fmt.Errorf("token: persist: %w", err)
	}
	return nil
}

// Revoke deletes the token with the given id. It is a no-op if absent.
func (st *Store) Revoke(ctx context.Context, id string) error {
	_, err := st.RevokeTree(ctx, id, nil)
	return err
}

// RevokeTree revokes the token id and, first, every token it created and theirs
// in turn (ADR D-022). cleanup, when non-nil, runs for each token before its
// record is deleted — to destroy what it owned (cubbyhole, database leases). It
// returns how many tokens were revoked. A token already gone is not an error.
func (st *Store) RevokeTree(ctx context.Context, id string, cleanup func(ctx context.Context, id string) error) (int, error) {
	return st.revokeTree(ctx, id, hashID(id), cleanup, 0)
}

// maxTreeDepth bounds the recursion; child chains are one parent deep per
// token/create call, so a real tree never approaches it.
const maxTreeDepth = 64

func (st *Store) revokeTree(ctx context.Context, id, hash string, cleanup func(ctx context.Context, id string) error, depth int) (int, error) {
	if depth > maxTreeDepth {
		return 0, fmt.Errorf("token: revoke: tree deeper than %d", maxTreeDepth)
	}
	revoked := 0

	children, err := st.store.List(ctx, childPrefix+hash+"/")
	if err != nil {
		return 0, fmt.Errorf("token: list children: %w", err)
	}
	for _, childHash := range children {
		entry, err := st.store.Get(ctx, storePrefix+childHash)
		if err != nil {
			return revoked, fmt.Errorf("token: read child: %w", err)
		}
		if entry != nil {
			var child Token
			if json.Unmarshal(entry.Value, &child) == nil && child.ID != "" {
				n, err := st.revokeTree(ctx, child.ID, childHash, cleanup, depth+1)
				revoked += n
				if err != nil {
					return revoked, err
				}
			}
		}
		if err := st.store.Delete(ctx, childKey(hash, childHash)); err != nil {
			return revoked, fmt.Errorf("token: unindex child: %w", err)
		}
	}

	entry, err := st.store.Get(ctx, storePrefix+hash)
	if err != nil {
		return revoked, fmt.Errorf("token: read: %w", err)
	}
	if entry == nil {
		return revoked, nil
	}
	var t Token
	_ = json.Unmarshal(entry.Value, &t)
	if cleanup != nil {
		if err := cleanup(ctx, id); err != nil {
			return revoked, err
		}
	}
	if err := st.store.Delete(ctx, storePrefix+hash); err != nil {
		return revoked, fmt.Errorf("token: revoke: %w", err)
	}
	if t.ParentHash != "" {
		_ = st.store.Delete(ctx, childKey(t.ParentHash, hash)) // best effort; a stale entry is skipped
	}
	return revoked + 1, nil
}

func childKey(parentHash, childHash string) string {
	return childPrefix + parentHash + "/" + childHash
}

// generateID returns a new random token value.
func generateID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("token: generate id: %w", err)
	}
	return displayPrefix + hex.EncodeToString(b), nil
}

// storeKey maps a token value to its storage key: the hash of the value, so the
// value itself never appears in an (unencrypted) key name.
func storeKey(id string) string {
	return storePrefix + hashID(id)
}

// hashID is the storage name for a token ID (see the package doc).
func hashID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}
