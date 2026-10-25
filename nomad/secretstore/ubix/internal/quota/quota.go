// Package quota implements Vault-compatible resource quotas. Phase 1 covers
// rate-limit quotas: path-prefix-scoped, API-managed request-rate limits, each
// backed by a per-client token bucket ([ratelimit.Limiter]) and persisted in the
// barrier. See docs/design/resource-quotas.md and ADR D-020.
//
// Enforcement is a hot path (every request), so quotas are held in memory; they
// are loaded from the barrier once after unseal ([Manager.EnsureLoaded]) and
// kept current by the write path ([Manager.Set]/[Manager.Delete]).
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cwolsen7905/ubixvault/internal/ratelimit"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// storePrefix is the barrier key prefix for rate-limit quota records.
const storePrefix = "sys/quotas/rate-limit/"

// configKey is the barrier key for the global quota config (the default quota).
const configKey = "sys/quotas/config"

// leaseStorePrefix is the barrier key prefix for lease-count quota records.
const leaseStorePrefix = "sys/quotas/lease-count/"

// Errors returned by the manager.
var (
	// ErrNotFound is returned for an unknown quota name.
	ErrNotFound = errors.New("quota: not found")
	// ErrInvalidName is returned for a name that is empty or not a single path-safe segment.
	ErrInvalidName = errors.New("quota: invalid name (must be a single path-safe segment)")
	// ErrInvalidRate is returned when rate is not positive.
	ErrInvalidRate = errors.New("quota: rate must be greater than 0")
	// ErrInvalidPath is returned for a path that is not a valid API path prefix.
	ErrInvalidPath = errors.New("quota: invalid path prefix")
	// ErrInvalidMax is returned when a lease-count quota's max is not positive.
	ErrInvalidMax = errors.New("quota: max_leases must be greater than 0")
)

// Storage is the subset of the barrier the manager needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// RateLimitQuota is a persisted, path-scoped request-rate limit.
type RateLimitQuota struct {
	Name string `json:"name"`
	// Path is the API path prefix the quota applies to; "" matches every path
	// (a global default). Matching is longest-prefix-wins.
	Path string `json:"path"`
	// Rate is the allowed requests per second per client; Burst is the most a
	// single client may spend at once (defaults to Rate when not positive).
	Rate  float64 `json:"rate"`
	Burst float64 `json:"burst"`
}

// effectiveBurst returns Burst, defaulting to Rate when unset.
func (q RateLimitQuota) effectiveBurst() float64 {
	if q.Burst > 0 {
		return q.Burst
	}
	return q.Rate
}

// LeaseCountQuota caps the number of active leases under a path prefix.
type LeaseCountQuota struct {
	Name string `json:"name"`
	// Path is the API path prefix the quota applies to; "" matches every path.
	Path string `json:"path"`
	// MaxLeases is the maximum number of active leases allowed under Path.
	MaxLeases int `json:"max_leases"`
}

type liveQuota struct {
	spec    RateLimitQuota
	limiter *ratelimit.Limiter
}

// rlConfig is the persisted global quota config (the default quota).
type rlConfig struct {
	DefaultRate  float64 `json:"default_rate"`
	DefaultBurst float64 `json:"default_burst"`
}

// Manager holds the rate-limit quotas and enforces them. Safe for concurrent use.
type Manager struct {
	store      Storage
	onExceeded func(name string) // optional metrics hook

	mu          sync.RWMutex
	quotas      map[string]*liveQuota       // named rate-limit quotas, by name
	leaseQuotas map[string]*LeaseCountQuota // lease-count quotas, by name
	loaded      bool
	// defaultLimiter is the global default quota applied to any path with no more
	// specific named quota. Seeded from the -rate-limit flag at startup and, when
	// present, overridden by the persisted config at unseal. nil disables it.
	defaultLimiter *ratelimit.Limiter
	defaultRate    float64
	defaultBurst   float64
	// seedLimiter is the flag-seeded default, restored by Reset so a persisted
	// config that has since been removed does not outlive a seal.
	seedLimiter *ratelimit.Limiter
}

// New returns a manager over store. onExceeded, if non-nil, is called with the
// quota name ("default" for the default quota) each time a request is denied
// (for metrics); it must not block.
func New(store Storage, onExceeded func(name string)) *Manager {
	return &Manager{
		store:       store,
		onExceeded:  onExceeded,
		quotas:      map[string]*liveQuota{},
		leaseQuotas: map[string]*LeaseCountQuota{},
	}
}

// SetDefaultLimiter installs l as the default (global) quota, applied when no
// named quota matches. Used to seed the default from the -rate-limit flag at
// startup; nil disables the default. Persisted config (loaded at unseal) takes
// precedence over this.
func (m *Manager) SetDefaultLimiter(l *ratelimit.Limiter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seedLimiter = l
	m.defaultLimiter = l
	if l != nil {
		m.defaultRate, m.defaultBurst = l.Rate(), l.Burst()
	} else {
		m.defaultRate, m.defaultBurst = 0, 0
	}
}

// setDefaultRate installs a default quota from rate/burst (burst defaults to
// rate). rate <= 0 clears the default. Caller holds no lock.
func (m *Manager) setDefaultRate(rate, burst float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rate <= 0 {
		m.defaultLimiter, m.defaultRate, m.defaultBurst = nil, 0, 0
		return
	}
	if burst <= 0 {
		burst = rate
	}
	m.defaultLimiter = ratelimit.New(rate, burst)
	m.defaultRate, m.defaultBurst = rate, burst
}

// DefaultConfig returns the current default quota's rate and burst (0, 0 when no
// default is set).
func (m *Manager) DefaultConfig() (rate, burst float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultRate, m.defaultBurst
}

// SetConfig persists the global quota config and applies the default quota. A
// non-positive rate clears the default.
func (m *Manager) SetConfig(ctx context.Context, rate, burst float64) error {
	blob, err := json.Marshal(rlConfig{DefaultRate: rate, DefaultBurst: burst})
	if err != nil {
		return fmt.Errorf("quota: encode config: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: configKey, Value: blob}); err != nil {
		return fmt.Errorf("quota: persist config: %w", err)
	}
	m.setDefaultRate(rate, burst)
	return nil
}

// Reset drops everything loaded from the barrier and restores the flag-seeded
// default, so the next [Manager.EnsureLoaded] reads storage afresh. Called when
// the vault is sealed.
func (m *Manager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotas = map[string]*liveQuota{}
	m.leaseQuotas = map[string]*LeaseCountQuota{}
	m.loaded = false
	m.defaultLimiter = m.seedLimiter
	if m.seedLimiter != nil {
		m.defaultRate, m.defaultBurst = m.seedLimiter.Rate(), m.seedLimiter.Burst()
	} else {
		m.defaultRate, m.defaultBurst = 0, 0
	}
}

// EnsureLoaded loads all persisted quotas into memory once. It is a no-op after a
// successful load, so callers may invoke it on the request path (guarded by the
// vault being unsealed). On error it leaves the manager unloaded so a later call
// retries.
func (m *Manager) EnsureLoaded(ctx context.Context) error {
	m.mu.RLock()
	loaded := m.loaded
	m.mu.RUnlock()
	if loaded {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded { // won the race
		return nil
	}
	names, err := m.store.List(ctx, storePrefix)
	if err != nil {
		return fmt.Errorf("quota: list: %w", err)
	}
	loadedQuotas := make(map[string]*liveQuota, len(names))
	for _, name := range names {
		entry, err := m.store.Get(ctx, storePrefix+name)
		if err != nil {
			return fmt.Errorf("quota: read %q: %w", name, err)
		}
		if entry == nil {
			continue
		}
		var q RateLimitQuota
		if err := json.Unmarshal(entry.Value, &q); err != nil {
			return fmt.Errorf("quota: decode %q: %w", name, err)
		}
		loadedQuotas[q.Name] = &liveQuota{spec: q, limiter: ratelimit.New(q.Rate, q.effectiveBurst())}
	}
	// Load lease-count quotas.
	lcNames, err := m.store.List(ctx, leaseStorePrefix)
	if err != nil {
		return fmt.Errorf("quota: list lease-count: %w", err)
	}
	loadedLease := make(map[string]*LeaseCountQuota, len(lcNames))
	for _, name := range lcNames {
		entry, err := m.store.Get(ctx, leaseStorePrefix+name)
		if err != nil {
			return fmt.Errorf("quota: read lease-count %q: %w", name, err)
		}
		if entry == nil {
			continue
		}
		var q LeaseCountQuota
		if err := json.Unmarshal(entry.Value, &q); err != nil {
			return fmt.Errorf("quota: decode lease-count %q: %w", name, err)
		}
		loadedLease[q.Name] = &q
	}

	m.quotas = loadedQuotas
	m.leaseQuotas = loadedLease
	m.loaded = true

	// Load the persisted config's default quota, if any; it overrides a
	// flag-seeded default. Done under the same lock via the fields directly.
	cfgEntry, err := m.store.Get(ctx, configKey)
	if err != nil {
		return fmt.Errorf("quota: read config: %w", err)
	}
	if cfgEntry != nil {
		var cfg rlConfig
		if err := json.Unmarshal(cfgEntry.Value, &cfg); err != nil {
			return fmt.Errorf("quota: decode config: %w", err)
		}
		if cfg.DefaultRate > 0 {
			burst := cfg.DefaultBurst
			if burst <= 0 {
				burst = cfg.DefaultRate
			}
			m.defaultLimiter = ratelimit.New(cfg.DefaultRate, burst)
			m.defaultRate, m.defaultBurst = cfg.DefaultRate, burst
		} else {
			m.defaultLimiter, m.defaultRate, m.defaultBurst = nil, 0, 0
		}
	}
	return nil
}

// Set creates or replaces a rate-limit quota: it validates, persists to the
// barrier, and updates the in-memory limiter.
func (m *Manager) Set(ctx context.Context, q RateLimitQuota) error {
	if !validName(q.Name) {
		return ErrInvalidName
	}
	if q.Rate <= 0 {
		return ErrInvalidRate
	}
	if !validPath(q.Path) {
		return ErrInvalidPath
	}
	blob, err := json.Marshal(q)
	if err != nil {
		return fmt.Errorf("quota: encode: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: storePrefix + q.Name, Value: blob}); err != nil {
		return fmt.Errorf("quota: persist: %w", err)
	}
	m.mu.Lock()
	m.quotas[q.Name] = &liveQuota{spec: q, limiter: ratelimit.New(q.Rate, q.effectiveBurst())}
	m.mu.Unlock()
	return nil
}

// Get returns the named quota's spec, or [ErrNotFound].
func (m *Manager) Get(name string) (RateLimitQuota, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	q, ok := m.quotas[name]
	if !ok {
		return RateLimitQuota{}, ErrNotFound
	}
	return q.spec, nil
}

// Delete removes the named quota. Absent is not an error.
func (m *Manager) Delete(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if err := m.store.Delete(ctx, storePrefix+name); err != nil {
		return fmt.Errorf("quota: delete: %w", err)
	}
	m.mu.Lock()
	delete(m.quotas, name)
	m.mu.Unlock()
	return nil
}

// List returns all quota specs, unordered.
func (m *Manager) List() []RateLimitQuota {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]RateLimitQuota, 0, len(m.quotas))
	for _, q := range m.quotas {
		out = append(out, q.spec)
	}
	return out
}

// SetLeaseCount creates or replaces a lease-count quota (validate, persist,
// update in memory).
func (m *Manager) SetLeaseCount(ctx context.Context, q LeaseCountQuota) error {
	if !validName(q.Name) {
		return ErrInvalidName
	}
	if q.MaxLeases <= 0 {
		return ErrInvalidMax
	}
	if !validPath(q.Path) {
		return ErrInvalidPath
	}
	blob, err := json.Marshal(q)
	if err != nil {
		return fmt.Errorf("quota: encode lease-count: %w", err)
	}
	if err := m.store.Put(ctx, &storage.Entry{Key: leaseStorePrefix + q.Name, Value: blob}); err != nil {
		return fmt.Errorf("quota: persist lease-count: %w", err)
	}
	m.mu.Lock()
	m.leaseQuotas[q.Name] = &q
	m.mu.Unlock()
	return nil
}

// GetLeaseCount returns the named lease-count quota, or [ErrNotFound].
func (m *Manager) GetLeaseCount(name string) (LeaseCountQuota, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	q, ok := m.leaseQuotas[name]
	if !ok {
		return LeaseCountQuota{}, ErrNotFound
	}
	return *q, nil
}

// DeleteLeaseCount removes the named lease-count quota. Absent is not an error.
func (m *Manager) DeleteLeaseCount(ctx context.Context, name string) error {
	if !validName(name) {
		return ErrInvalidName
	}
	if err := m.store.Delete(ctx, leaseStorePrefix+name); err != nil {
		return fmt.Errorf("quota: delete lease-count: %w", err)
	}
	m.mu.Lock()
	delete(m.leaseQuotas, name)
	m.mu.Unlock()
	return nil
}

// ListLeaseCount returns all lease-count quota specs, unordered.
func (m *Manager) ListLeaseCount() []LeaseCountQuota {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]LeaseCountQuota, 0, len(m.leaseQuotas))
	for _, q := range m.leaseQuotas {
		out = append(out, *q)
	}
	return out
}

// MatchLeaseCount returns the single most-specific lease-count quota (longest
// path prefix) matching path, if any. The caller counts active leases under the
// returned quota's Path and rejects a new lease when the count is at the max.
func (m *Manager) MatchLeaseCount(path string) (LeaseCountQuota, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *LeaseCountQuota
	bestLen := -1
	for _, q := range m.leaseQuotas {
		if pathMatches(q.Path, path) && len(q.Path) > bestLen {
			best, bestLen = q, len(q.Path)
		}
	}
	if best == nil {
		return LeaseCountQuota{}, false
	}
	return *best, true
}

// Allow reports whether a request to path from client may proceed. It applies
// the single most-specific matching quota (longest path prefix); a request with
// no matching quota is always allowed. When a quota denies the request, its name
// is returned and the onExceeded hook (if any) is called.
func (m *Manager) Allow(path, client string) (allowed bool, quota string) {
	m.mu.RLock()
	var best *liveQuota
	bestLen := -1
	for _, q := range m.quotas {
		if !pathMatches(q.spec.Path, path) {
			continue
		}
		if len(q.spec.Path) > bestLen {
			best, bestLen = q, len(q.spec.Path)
		}
	}
	def := m.defaultLimiter
	m.mu.RUnlock()

	// A named quota is the most specific match and governs the request alone.
	if best != nil {
		if best.limiter.Allow(client) {
			return true, best.spec.Name
		}
		if m.onExceeded != nil {
			m.onExceeded(best.spec.Name)
		}
		return false, best.spec.Name
	}
	// Otherwise fall back to the default (global) quota, when one is set.
	if def != nil {
		if def.Allow(client) {
			return true, ""
		}
		if m.onExceeded != nil {
			m.onExceeded("default")
		}
		return false, ""
	}
	return true, ""
}

// pathMatches reports whether quotaPath is a prefix of reqPath. An empty
// quotaPath matches everything (global).
func pathMatches(quotaPath, reqPath string) bool {
	return quotaPath == "" || strings.HasPrefix(reqPath, quotaPath)
}

// validName restricts quota names to a single path-safe segment (as policies).
func validName(name string) bool {
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	return storage.ValidateKey(storePrefix+name) == nil
}

// validPath allows an empty prefix (global) or any prefix without control
// characters; it is a match prefix for request paths, not a storage key.
func validPath(p string) bool {
	return !strings.ContainsAny(p, "\x00\n\r")
}
