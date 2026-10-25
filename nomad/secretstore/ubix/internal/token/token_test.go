package token

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newStore() (*Store, *storage.MemoryBackend) {
	mem := storage.NewMemoryBackend()
	return NewStore(mem), mem
}

// clockedStore returns a store whose clock the caller controls via the returned
// pointer, plus its backend.
func clockedStore() (*Store, *storage.MemoryBackend, *time.Time) {
	mem := storage.NewMemoryBackend()
	st := NewStore(mem)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	return st, mem, &now
}

func TestCreateRootAndLookup(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore()

	root, err := st.CreateRoot(ctx)
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	if !root.IsRoot() {
		t.Fatal("root token is not root")
	}
	if !strings.HasPrefix(root.ID, displayPrefix) {
		t.Fatalf("token id %q lacks prefix %q", root.ID, displayPrefix)
	}

	got, err := st.Lookup(ctx, root.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ID != root.ID || !got.IsRoot() {
		t.Fatalf("looked-up token = %+v", got)
	}
}

func TestCreateWithPolicies(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore()

	tok, err := st.Create(ctx, []string{"read-only", "app"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tok.IsRoot() {
		t.Fatal("non-root token reports root")
	}
	got, _ := st.Lookup(ctx, tok.ID)
	if len(got.Policies) != 2 || got.Policies[0] != "read-only" {
		t.Fatalf("policies = %v", got.Policies)
	}
}

func TestLookupUnknown(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore()
	if _, err := st.Lookup(ctx, "uv.does-not-exist"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("want ErrTokenNotFound, got %v", err)
	}
}

func TestRevoke(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore()
	tok, _ := st.CreateRoot(ctx)

	if err := st.Revoke(ctx, tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := st.Lookup(ctx, tok.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("after revoke: want ErrTokenNotFound, got %v", err)
	}
	// Revoking again is a no-op.
	if err := st.Revoke(ctx, tok.ID); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
}

func TestTokensAreUnique(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore()
	a, _ := st.CreateRoot(ctx)
	b, _ := st.CreateRoot(ctx)
	if a.ID == b.ID {
		t.Fatal("two tokens share an id")
	}
}

// TestTokenValueNotInStorageKey is the anti-leak guarantee: because the barrier
// does not encrypt key names, the token value must never appear in a storage
// key. Records are indexed by the hash of the token instead.
func TestTokenValueNotInStorageKey(t *testing.T) {
	ctx := context.Background()
	st, mem := newStore()
	tok, _ := st.CreateRoot(ctx)

	keys, err := mem.List(ctx, storePrefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	rawID := strings.TrimPrefix(tok.ID, displayPrefix)
	for _, k := range keys {
		if strings.Contains(k, rawID) || strings.Contains(k, tok.ID) {
			t.Fatalf("token value leaked into storage key %q", k)
		}
	}
	// And the token is still retrievable by its value.
	if _, err := st.Lookup(ctx, tok.ID); err != nil {
		t.Fatalf("Lookup after key check: %v", err)
	}
}

func TestCreateAppliesDefaultTTL(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore()
	tok, err := st.Create(ctx, []string{"p"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("Create should set an expiry (default TTL)")
	}
}

func TestRootNeverExpires(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()
	root, _ := st.CreateRoot(ctx)
	if !root.ExpiresAt.IsZero() {
		t.Fatalf("root token has an expiry: %v", root.ExpiresAt)
	}
	// Far in the future, the root token is still valid.
	*now = now.AddDate(10, 0, 0)
	if _, err := st.Lookup(ctx, root.ID); err != nil {
		t.Fatalf("root token expired: %v", err)
	}
}

func TestCreateWithTTL(t *testing.T) {
	ctx := context.Background()
	st, _, _ := clockedStore()

	ttlTok, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour)
	if ttlTok.ExpiresAt.IsZero() {
		t.Fatal("CreateWithTTL(1h) should set an expiry")
	}
	neverTok, _ := st.CreateWithTTL(ctx, []string{"p"}, 0)
	if !neverTok.ExpiresAt.IsZero() {
		t.Fatal("CreateWithTTL(0) should never expire")
	}
}

// An expired token is refused at once; its record is removed by the sweeper
// (which also revokes its children), not by the lookup.
func TestLookupExpiredTokenIsRefusedThenSwept(t *testing.T) {
	ctx := context.Background()
	st, mem, now := clockedStore()

	tok, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour)
	*now = now.Add(2 * time.Hour) // past expiry

	if _, err := st.Lookup(ctx, tok.ID); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired lookup: want ErrTokenExpired, got %v", err)
	}
	if n, err := st.SweepExpired(ctx, 0, nil); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1", n, err)
	}
	if raw, _ := mem.Get(ctx, storeKey(tok.ID)); raw != nil {
		t.Fatal("expired token record not removed by the sweep")
	}
}

func TestRenewExtendsExpiry(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()

	tok, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour) // expires at base+1h
	*now = now.Add(30 * time.Minute)                          // still valid

	renewed, err := st.Renew(ctx, tok.ID, 2*time.Hour)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	want := now.Add(2 * time.Hour)
	if !renewed.ExpiresAt.Equal(want) {
		t.Fatalf("renewed expiry = %v, want %v", renewed.ExpiresAt, want)
	}
	// After the original expiry it is still valid thanks to the renewal.
	*now = now.Add(90 * time.Minute)
	if _, err := st.Lookup(ctx, tok.ID); err != nil {
		t.Fatalf("token expired despite renewal: %v", err)
	}
}

func TestRenewNonExpiringIsNoOp(t *testing.T) {
	ctx := context.Background()
	st, _, _ := clockedStore()
	root, _ := st.CreateRoot(ctx)
	renewed, err := st.Renew(ctx, root.ID, time.Hour)
	if err != nil {
		t.Fatalf("Renew root: %v", err)
	}
	if !renewed.ExpiresAt.IsZero() {
		t.Fatal("renewing a non-expiring token gave it an expiry")
	}
}

// Renewal cannot push a short-lived token past the system maximum: the hole this
// closes is a 1h token renewing itself with a 10-year increment.
func TestRenewCappedAtSystemMax(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()
	base := *now

	tok, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour)
	if want := base.Add(DefaultMaxTTL); !tok.MaxExpiresAt.Equal(want) {
		t.Fatalf("MaxExpiresAt = %v, want created+DefaultMaxTTL %v", tok.MaxExpiresAt, want)
	}
	renewed, err := st.Renew(ctx, tok.ID, 87600*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := base.Add(DefaultMaxTTL); !renewed.ExpiresAt.Equal(want) {
		t.Fatalf("renewed to %v, want capped at %v", renewed.ExpiresAt, want)
	}
	// Past the ceiling the token is gone, however often it renewed.
	*now = base.Add(DefaultMaxTTL + time.Second)
	if _, err := st.Lookup(ctx, tok.ID); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Lookup past ceiling = %v, want ErrTokenExpired", err)
	}
}

// A token deliberately issued for longer than the system maximum (e.g. a 1-year
// CI token) keeps its full lifetime: the ceiling is the later of the two.
func TestLongTTLTokenKeepsItsLifetime(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()
	year := 8760 * time.Hour

	tok, _ := st.CreateWithTTL(ctx, []string{"ci"}, year)
	if want := now.Add(year); !tok.ExpiresAt.Equal(want) || !tok.MaxExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt=%v MaxExpiresAt=%v, want both %v", tok.ExpiresAt, tok.MaxExpiresAt, want)
	}
	renewed, _ := st.Renew(ctx, tok.ID, 2*year)
	if !renewed.ExpiresAt.Equal(tok.MaxExpiresAt) {
		t.Fatalf("renewed to %v, want capped at its own ceiling %v", renewed.ExpiresAt, tok.MaxExpiresAt)
	}
}

func TestSetMaxTTL(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()
	st.SetMaxTTL(2 * time.Hour)
	tok, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour)
	if want := now.Add(2 * time.Hour); !tok.MaxExpiresAt.Equal(want) {
		t.Fatalf("MaxExpiresAt = %v, want %v", tok.MaxExpiresAt, want)
	}
	st.SetMaxTTL(0) // restores the default
	if st.maxTTL != DefaultMaxTTL {
		t.Fatalf("SetMaxTTL(0) left maxTTL = %v", st.maxTTL)
	}
}

// A token stored before ceilings existed (no MaxExpiresAt) is never shortened:
// its derived ceiling is the later of its current expiry and created+max, and
// the first renewal records it so it cannot drift.
func TestLegacyTokenCeiling(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()
	base := *now

	// Simulate a pre-upgrade record: a 1-year token with no ceiling stored.
	tok, _ := st.CreateWithTTL(ctx, []string{"legacy"}, 8760*time.Hour)
	tok.MaxExpiresAt = time.Time{}
	if err := st.save(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if got := st.MaxExpiry(tok); !got.Equal(tok.ExpiresAt) {
		t.Fatalf("legacy MaxExpiry = %v, want its current expiry %v", got, tok.ExpiresAt)
	}
	renewed, _ := st.Renew(ctx, tok.ID, time.Hour)
	if !renewed.MaxExpiresAt.Equal(base.Add(8760 * time.Hour)) {
		t.Fatalf("recorded ceiling = %v, want %v", renewed.MaxExpiresAt, base.Add(8760*time.Hour))
	}

	// A short legacy token gets created+max, like a new one would.
	short, _ := st.CreateWithTTL(ctx, []string{"legacy"}, time.Hour)
	short.MaxExpiresAt = time.Time{}
	if got := st.MaxExpiry(short); !got.Equal(base.Add(DefaultMaxTTL)) {
		t.Fatalf("short legacy MaxExpiry = %v, want %v", got, base.Add(DefaultMaxTTL))
	}
}

func TestCreateChildCannotOutliveParent(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()
	parent, _ := st.CreateWithTTL(ctx, []string{"p"}, 2*time.Hour)
	parent.MaxExpiresAt = now.Add(2 * time.Hour) // a parent with a 2h ceiling
	if err := st.save(ctx, parent); err != nil {
		t.Fatal(err)
	}
	bound := now.Add(2 * time.Hour)

	child, err := st.CreateChild(ctx, parent, []string{"p"}, 876000*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !child.ExpiresAt.Equal(bound) || !child.MaxExpiresAt.Equal(bound) {
		t.Fatalf("ExpiresAt=%v MaxExpiresAt=%v, want both clamped to %v", child.ExpiresAt, child.MaxExpiresAt, bound)
	}
	if child.IsOrphan() || child.ParentHash != hashID(parent.ID) {
		t.Fatalf("child ParentHash = %q, want the parent's hash", child.ParentHash)
	}
	if strings.Contains(child.ParentHash, parent.ID) {
		t.Fatal("child record holds the parent's raw token ID")
	}
	renewed, _ := st.Renew(ctx, child.ID, 24*time.Hour)
	if renewed.ExpiresAt.After(bound) {
		t.Fatalf("renewed past bound: %v > %v", renewed.ExpiresAt, bound)
	}

	// Inside the bound, the requested TTL applies as usual.
	short, _ := st.CreateChild(ctx, parent, []string{"p"}, 30*time.Minute)
	if want := now.Add(30 * time.Minute); !short.ExpiresAt.Equal(want) {
		t.Fatalf("short child expiry = %v, want %v", short.ExpiresAt, want)
	}
}

// Revoking a token revokes the tokens it created and theirs, running cleanup for
// each; revoking a child leaves its parent; orphans are never swept up.
func TestRevokeTree(t *testing.T) {
	ctx := context.Background()
	st, mem, _ := clockedStore()

	root, _ := st.CreateRoot(ctx)
	parent, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour) // an orphan (root-issued)
	child, _ := st.CreateChild(ctx, parent, []string{"p"}, time.Hour)
	grandchild, _ := st.CreateChild(ctx, child, []string{"p"}, time.Hour)
	sibling, _ := st.CreateChild(ctx, parent, []string{"p"}, time.Hour)
	bystander, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour)

	// Revoking a leaf leaves the rest.
	if n, err := st.RevokeTree(ctx, sibling.ID, nil); err != nil || n != 1 {
		t.Fatalf("revoke leaf = %d, %v; want 1", n, err)
	}
	if _, err := st.Lookup(ctx, parent.ID); err != nil {
		t.Fatalf("parent gone after revoking a child: %v", err)
	}

	var cleaned []string
	n, err := st.RevokeTree(ctx, parent.ID, func(_ context.Context, id string) error {
		cleaned = append(cleaned, id)
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("revoke parent = %d, %v; want 3 (parent, child, grandchild)", n, err)
	}
	// Children are cleaned up before their parents.
	if len(cleaned) != 3 || cleaned[0] != grandchild.ID || cleaned[2] != parent.ID {
		t.Fatalf("cleanup order = %d ids, want grandchild first and parent last", len(cleaned))
	}
	for _, tok := range []*Token{parent, child, grandchild} {
		if _, err := st.Lookup(ctx, tok.ID); !errors.Is(err, ErrTokenNotFound) {
			t.Errorf("token still present after cascade: %v", err)
		}
	}
	for _, tok := range []*Token{root, bystander} {
		if _, err := st.Lookup(ctx, tok.ID); err != nil {
			t.Errorf("unrelated token revoked by the cascade: %v", err)
		}
	}
	// No index entries left behind.
	if left, _ := mem.List(ctx, childPrefix); len(left) != 0 {
		t.Fatalf("child index not cleaned up: %v", left)
	}
}

// A parent that expires takes its children with it on the next sweep.
func TestSweepCascadesToChildren(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()

	parent, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour)
	parent.MaxExpiresAt = now.Add(10 * time.Hour)
	_ = st.save(ctx, parent)
	child, _ := st.CreateChild(ctx, parent, []string{"p"}, 5*time.Hour) // outlives the parent's current expiry

	*now = now.Add(2 * time.Hour) // parent expired, child not
	if _, err := st.Lookup(ctx, child.ID); err != nil {
		t.Fatalf("child should still be valid before the sweep: %v", err)
	}
	if n, err := st.SweepExpired(ctx, 0, nil); err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v; want 2 (the expired parent and its child)", n, err)
	}
	if _, err := st.Lookup(ctx, child.ID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("child survived its expired parent: %v", err)
	}
}

func TestSweepExpired(t *testing.T) {
	ctx := context.Background()
	st, mem, now := clockedStore()

	live, _ := st.CreateWithTTL(ctx, []string{"p"}, 2*time.Hour)
	root, _ := st.CreateRoot(ctx)
	var expired []*Token
	for i := 0; i < 3; i++ {
		tok, _ := st.CreateWithTTL(ctx, []string{"p"}, time.Hour)
		expired = append(expired, tok)
	}
	*now = now.Add(90 * time.Minute) // the 1h tokens are past expiry; the 2h one is not

	var cleaned []string
	n, err := st.SweepExpired(ctx, 0, func(_ context.Context, id string) error {
		cleaned = append(cleaned, id)
		return nil
	})
	if err != nil || n != 3 || len(cleaned) != 3 {
		t.Fatalf("SweepExpired = %d, %v; cleaned %d, want 3", n, err, len(cleaned))
	}
	for _, tok := range expired {
		if e, _ := mem.Get(ctx, storeKey(tok.ID)); e != nil {
			t.Errorf("expired token %s still stored", tok.ID[:8])
		}
	}
	for _, tok := range []*Token{live, root} {
		if _, err := st.Lookup(ctx, tok.ID); err != nil {
			t.Errorf("unexpired token swept: %v", err)
		}
	}
}

func TestSweepExpiredLimitAndFailedCleanup(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()
	for i := 0; i < 5; i++ {
		_, _ = st.CreateWithTTL(ctx, []string{"p"}, time.Minute)
	}
	*now = now.Add(time.Hour)

	if n, _ := st.SweepExpired(ctx, 2, nil); n != 2 {
		t.Fatalf("limited sweep removed %d, want 2", n)
	}
	// A failing cleanup keeps the record for the next sweep.
	if n, _ := st.SweepExpired(ctx, 0, func(context.Context, string) error { return errors.New("sealed") }); n != 0 {
		t.Fatalf("sweep with failing cleanup removed %d, want 0", n)
	}
	if n, _ := st.SweepExpired(ctx, 0, nil); n != 3 {
		t.Fatalf("follow-up sweep removed %d, want the remaining 3", n)
	}
}

// A role's maxTTL lowers the token's ceiling: expiry and renewal stop there.
func TestCreateForLoginRoleMaxTTL(t *testing.T) {
	ctx := context.Background()
	st, _, now := clockedStore()

	tok, err := st.CreateForLogin(ctx, []string{"p"}, 2*time.Hour, time.Hour, "userpass", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(time.Hour); !tok.ExpiresAt.Equal(want) || !tok.MaxExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt=%v MaxExpiresAt=%v, want both at the role max %v", tok.ExpiresAt, tok.MaxExpiresAt, want)
	}
	renewed, _ := st.Renew(ctx, tok.ID, 87600*time.Hour)
	if renewed.ExpiresAt.After(now.Add(time.Hour)) {
		t.Fatalf("renewed past the role max: %v", renewed.ExpiresAt)
	}

	// Without a role max, the usual ceiling (own TTL vs system maximum) applies.
	plain, _ := st.CreateForLogin(ctx, []string{"p"}, time.Hour, 0, "userpass", "", nil)
	if want := now.Add(DefaultMaxTTL); !plain.MaxExpiresAt.Equal(want) {
		t.Fatalf("no role max: MaxExpiresAt=%v, want %v", plain.MaxExpiresAt, want)
	}
	// ttl <= 0 means the default TTL, still under the role max.
	def, _ := st.CreateForLogin(ctx, []string{"p"}, 0, 30*time.Minute, "userpass", "", nil)
	if want := now.Add(30 * time.Minute); !def.ExpiresAt.Equal(want) {
		t.Fatalf("default ttl under a 30m max: ExpiresAt=%v, want %v", def.ExpiresAt, want)
	}
}
