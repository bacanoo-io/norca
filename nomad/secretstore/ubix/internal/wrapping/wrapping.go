// Package wrapping implements response wrapping (cubbyhole-style): a caller wraps
// an arbitrary JSON payload and receives a single-use, TTL'd wrapping token. The
// holder of that token can unwrap the payload exactly once, after which the token
// is destroyed. This is the "secure introduction" pattern — hand a secret to a
// consumer through a short-lived token instead of the secret itself, so the
// secret is never in transit or logs in the clear and a leaked token is either
// unused (still safe to rotate) or already burned.
//
// Wrapped payloads are stored through the barrier (encrypted at rest) and indexed
// by the SHA-256 of the wrapping token, never by the token value — the same
// discipline the token store uses, since the barrier encrypts values but not key
// names.
package wrapping

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

const (
	storePrefix   = "sys/wrapping/"
	displayPrefix = "uvw." // marks a wrapping token; distinct from auth tokens (uv.)
	tokenBytes    = 24
)

// DefaultTTL applies when a wrap request gives no positive TTL. MaxTTL caps how
// long a wrapped payload may live.
const (
	DefaultTTL = 5 * time.Minute
	MaxTTL     = 24 * time.Hour
)

// Errors.
var (
	ErrNotFound   = errors.New("wrapping: token not found")
	ErrExpired    = errors.New("wrapping: token expired")
	ErrInvalidTTL = errors.New("wrapping: ttl exceeds maximum")
)

// Storage is the subset of a backend the store needs; *barrier.Barrier satisfies it.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
}

// wrapped is the persisted record behind a wrapping token.
type wrapped struct {
	Payload     json.RawMessage `json:"payload"`
	CreatedTime time.Time       `json:"created_time"`
	ExpiresAt   time.Time       `json:"expires_at"`
}

// Info is the non-secret result of wrapping.
type Info struct {
	Token        string
	TTL          time.Duration
	CreationTime time.Time
	ExpiresAt    time.Time
}

// Store persists wrapped payloads.
type Store struct {
	store Storage
	now   func() time.Time

	// unwrapMu makes Unwrap's read-then-delete atomic. Storage has no
	// compare-and-delete, so without it concurrent unwraps of one token can all
	// read the payload before any of them deletes it. It is per process: across
	// replicas, single-use rests on there being one active writer (ADR D-021).
	unwrapMu sync.Mutex
}

// NewStore returns a wrapping store over s.
func NewStore(s Storage) *Store {
	return &Store{store: s, now: func() time.Time { return time.Now().UTC() }}
}

// Wrap stores payload under a fresh single-use token that expires after ttl
// (DefaultTTL if ttl <= 0). It fails with [ErrInvalidTTL] if ttl exceeds MaxTTL.
func (s *Store) Wrap(ctx context.Context, payload json.RawMessage, ttl time.Duration) (*Info, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		return nil, ErrInvalidTTL
	}
	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	now := s.now()
	rec := wrapped{Payload: payload, CreatedTime: now, ExpiresAt: now.Add(ttl)}
	blob, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("wrapping: marshal: %w", err)
	}
	if err := s.store.Put(ctx, &storage.Entry{Key: storeKey(token), Value: blob}); err != nil {
		return nil, fmt.Errorf("wrapping: persist: %w", err)
	}
	return &Info{Token: token, TTL: ttl, CreationTime: now, ExpiresAt: rec.ExpiresAt}, nil
}

// Unwrap returns the payload for token and destroys it, so a second unwrap of the
// same token fails with [ErrNotFound]. An expired token returns [ErrExpired]
// (and is deleted); an unknown token returns [ErrNotFound].
func (s *Store) Unwrap(ctx context.Context, token string) (json.RawMessage, error) {
	s.unwrapMu.Lock()
	defer s.unwrapMu.Unlock()

	key := storeKey(token)
	entry, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("wrapping: lookup: %w", err)
	}
	if entry == nil {
		return nil, ErrNotFound
	}
	var rec wrapped
	if err := json.Unmarshal(entry.Value, &rec); err != nil {
		return nil, fmt.Errorf("wrapping: unmarshal: %w", err)
	}
	if s.now().After(rec.ExpiresAt) {
		_ = s.store.Delete(ctx, key) // best-effort cleanup
		return nil, ErrExpired
	}
	// Single-use: destroy before returning so the token can't be replayed.
	if err := s.store.Delete(ctx, key); err != nil {
		return nil, fmt.Errorf("wrapping: consume: %w", err)
	}
	return rec.Payload, nil
}

func generateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("wrapping: generate token: %w", err)
	}
	return displayPrefix + hex.EncodeToString(b), nil
}

// storeKey maps a wrapping token to its storage key: the hash of the value, so
// the token itself never appears in an (unencrypted) key name.
func storeKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return storePrefix + hex.EncodeToString(sum[:])
}
