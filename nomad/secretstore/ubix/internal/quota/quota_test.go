package quota

import (
	"context"
	"strings"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/ratelimit"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// memStore is a minimal in-memory Storage for tests. List returns the immediate
// child names under a prefix, mirroring the barrier's list semantics.
type memStore struct {
	m map[string][]byte
}

func newMemStore() *memStore { return &memStore{m: map[string][]byte{}} }

func (s *memStore) Get(_ context.Context, key string) (*storage.Entry, error) {
	v, ok := s.m[key]
	if !ok {
		return nil, nil
	}
	return &storage.Entry{Key: key, Value: v}, nil
}
func (s *memStore) Put(_ context.Context, e *storage.Entry) error { s.m[e.Key] = e.Value; return nil }
func (s *memStore) Delete(_ context.Context, key string) error    { delete(s.m, key); return nil }
func (s *memStore) List(_ context.Context, prefix string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for k := range s.m {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i]
		}
		if rest != "" && !seen[rest] {
			seen[rest] = true
			names = append(names, rest)
		}
	}
	return names, nil
}

func TestManager_SetGetListDelete(t *testing.T) {
	ctx := context.Background()
	m := New(newMemStore(), nil)

	if err := m.Set(ctx, RateLimitQuota{Name: "api", Path: "secret/", Rate: 5}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := m.Get("api")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Path != "secret/" || got.Rate != 5 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if l := m.List(); len(l) != 1 {
		t.Errorf("list len = %d, want 1", len(l))
	}
	if err := m.Delete(ctx, "api"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.Get("api"); err != ErrNotFound {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
}

func TestManager_EnsureLoaded_Persists(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m1 := New(store, nil)
	if err := m1.Set(ctx, RateLimitQuota{Name: "global", Path: "", Rate: 10, Burst: 20}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// A fresh manager over the same store must recover the quota after loading.
	m2 := New(store, nil)
	if err := m2.EnsureLoaded(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	got, err := m2.Get("global")
	if err != nil {
		t.Fatalf("get after load: %v", err)
	}
	if got.Rate != 10 || got.Burst != 20 {
		t.Errorf("loaded quota mismatch: %+v", got)
	}
}

// TestManager_Reset: after Reset (a seal) the manager forgets what it loaded and
// falls back to the flag-seeded default, so the next load reflects storage as it
// is then — including quotas and config another writer has since removed.
func TestManager_Reset(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m := New(store, nil)
	m.SetDefaultLimiter(ratelimit.New(5, 5)) // the -rate-limit flag
	if err := m.Set(ctx, RateLimitQuota{Name: "api", Path: "secret/", Rate: 10}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := m.SetConfig(ctx, 50, 50); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if err := m.EnsureLoaded(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Another writer removes both.
	store.m = map[string][]byte{}

	m.Reset()
	if rate, burst := m.DefaultConfig(); rate != 5 || burst != 5 {
		t.Fatalf("default after Reset = %v/%v, want the flag-seeded 5/5", rate, burst)
	}
	if _, err := m.Get("api"); err != ErrNotFound {
		t.Fatalf("Get after Reset = %v, want ErrNotFound", err)
	}
	if err := m.EnsureLoaded(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := m.Get("api"); err != ErrNotFound {
		t.Fatalf("Get after reload = %v, want ErrNotFound (deleted in storage)", err)
	}
	if rate, _ := m.DefaultConfig(); rate != 5 {
		t.Fatalf("default after reload = %v, want 5", rate)
	}
}

func TestManager_Allow_LongestPrefixWins(t *testing.T) {
	ctx := context.Background()
	m := New(newMemStore(), nil)
	// Global generous, a stricter one on secret/, and a strictest on secret/db/.
	mustSet(t, m, RateLimitQuota{Name: "global", Path: "", Rate: 1000, Burst: 1000})
	mustSet(t, m, RateLimitQuota{Name: "secret", Path: "/v1/secret/", Rate: 1000, Burst: 1000})
	mustSet(t, m, RateLimitQuota{Name: "secretdb", Path: "/v1/secret/db/", Rate: 1, Burst: 2})
	_ = ctx

	// A request under the strictest prefix is governed by "secretdb".
	if _, q := m.Allow("/v1/secret/db/creds", "10.0.0.1"); q != "secretdb" {
		t.Errorf("longest-prefix match = %q, want secretdb", q)
	}
	// A request under secret/ but not secret/db/ is governed by "secret".
	if _, q := m.Allow("/v1/secret/data/x", "10.0.0.1"); q != "secret" {
		t.Errorf("match = %q, want secret", q)
	}
	// An unrelated path falls to the global quota.
	if _, q := m.Allow("/v1/transit/keys", "10.0.0.1"); q != "global" {
		t.Errorf("match = %q, want global", q)
	}
}

func TestManager_Allow_EnforcesAndCountsExceeded(t *testing.T) {
	var exceeded []string
	m := New(newMemStore(), func(name string) { exceeded = append(exceeded, name) })
	mustSet(t, m, RateLimitQuota{Name: "strict", Path: "/v1/secret/", Rate: 1, Burst: 2})

	// Burst is 2: two immediate requests pass, the third (within the same second)
	// is denied.
	client := "10.0.0.9"
	if ok, _ := m.Allow("/v1/secret/x", client); !ok {
		t.Fatal("request 1 should pass")
	}
	if ok, _ := m.Allow("/v1/secret/x", client); !ok {
		t.Fatal("request 2 should pass")
	}
	ok, q := m.Allow("/v1/secret/x", client)
	if ok {
		t.Fatal("request 3 should be denied (burst exhausted)")
	}
	if q != "strict" {
		t.Errorf("denied quota = %q, want strict", q)
	}
	if len(exceeded) != 1 || exceeded[0] != "strict" {
		t.Errorf("onExceeded = %v, want [strict]", exceeded)
	}

	// A different client has its own bucket and is not affected.
	if ok, _ := m.Allow("/v1/secret/x", "10.0.0.10"); !ok {
		t.Error("a different client should not be throttled by another's usage")
	}
}

func TestManager_Allow_NoMatchAllows(t *testing.T) {
	m := New(newMemStore(), nil)
	mustSet(t, m, RateLimitQuota{Name: "secret", Path: "/v1/secret/", Rate: 1, Burst: 1})
	if ok, q := m.Allow("/v1/transit/keys", "1.2.3.4"); !ok || q != "" {
		t.Errorf("no-match should allow with empty quota; got ok=%v q=%q", ok, q)
	}
}

func TestManager_SetValidation(t *testing.T) {
	ctx := context.Background()
	m := New(newMemStore(), nil)
	cases := map[string]struct {
		q    RateLimitQuota
		want error
	}{
		"empty name":    {RateLimitQuota{Name: "", Rate: 1}, ErrInvalidName},
		"slashed name":  {RateLimitQuota{Name: "a/b", Rate: 1}, ErrInvalidName},
		"zero rate":     {RateLimitQuota{Name: "x", Rate: 0}, ErrInvalidRate},
		"negative rate": {RateLimitQuota{Name: "x", Rate: -1}, ErrInvalidRate},
		"bad path":      {RateLimitQuota{Name: "x", Rate: 1, Path: "a\nb"}, ErrInvalidPath},
	}
	for name, tc := range cases {
		if err := m.Set(ctx, tc.q); err != tc.want {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func mustSet(t *testing.T, m *Manager, q RateLimitQuota) {
	t.Helper()
	if err := m.Set(context.Background(), q); err != nil {
		t.Fatalf("set %q: %v", q.Name, err)
	}
}

func TestManager_LeaseCount_CRUDAndMatch(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	m := New(store, nil)

	if err := m.SetLeaseCount(ctx, LeaseCountQuota{Name: "db", Path: "database/", MaxLeases: 100}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := m.SetLeaseCount(ctx, LeaseCountQuota{Name: "role", Path: "database/creds/app", MaxLeases: 5}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := m.GetLeaseCount("role"); err != nil || got.MaxLeases != 5 {
		t.Fatalf("get role: %+v err=%v", got, err)
	}
	if l := m.ListLeaseCount(); len(l) != 2 {
		t.Errorf("list len = %d, want 2", len(l))
	}

	// Longest-prefix match: creds/app path resolves to the role quota; another
	// role path resolves to the mount-wide quota.
	if q, ok := m.MatchLeaseCount("database/creds/app"); !ok || q.Name != "role" {
		t.Errorf("match creds/app = %q ok=%v, want role", q.Name, ok)
	}
	if q, ok := m.MatchLeaseCount("database/creds/other"); !ok || q.Name != "db" {
		t.Errorf("match creds/other = %q ok=%v, want db", q.Name, ok)
	}
	if _, ok := m.MatchLeaseCount("transit/keys"); ok {
		t.Error("unrelated path should not match a database lease-count quota")
	}

	// Persistence: a fresh manager recovers them.
	m2 := New(store, nil)
	if err := m2.EnsureLoaded(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	if l := m2.ListLeaseCount(); len(l) != 2 {
		t.Errorf("reloaded list len = %d, want 2", len(l))
	}

	if err := m.SetLeaseCount(ctx, LeaseCountQuota{Name: "bad", Path: "database/", MaxLeases: 0}); err != ErrInvalidMax {
		t.Errorf("zero max err = %v, want ErrInvalidMax", err)
	}
	if err := m.DeleteLeaseCount(ctx, "db"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.GetLeaseCount("db"); err != ErrNotFound {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
}
