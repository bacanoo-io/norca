package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// mockPlugin records the calls made to it, standing in for a real database.
type mockPlugin struct {
	initURL   string
	closes    int
	created   []CreateUserRequest
	revoked   []string
	createErr error
}

func (m *mockPlugin) Initialize(_ context.Context, url string) error { m.initURL = url; return nil }
func (m *mockPlugin) CreateUser(_ context.Context, req CreateUserRequest) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.created = append(m.created, req)
	return nil
}
func (m *mockPlugin) RevokeUser(_ context.Context, username string) error {
	m.revoked = append(m.revoked, username)
	return nil
}
func (m *mockPlugin) Close() error { m.closes++; return nil }

func newEngine(t *testing.T) (*Engine, *mockPlugin) {
	t.Helper()
	p := &mockPlugin{}
	return New(storage.NewMemoryBackend(), "database", p), p
}

// configured returns an engine that has been configured and given a role.
func configured(t *testing.T) (*Engine, *mockPlugin) {
	t.Helper()
	e, p := newEngine(t)
	ctx := context.Background()
	if err := e.Configure(ctx, "mariadb://root:pw@localhost/db"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	role := Role{
		CreationStatements: []string{"CREATE USER '{{username}}' IDENTIFIED BY '{{password}}';"},
		DefaultTTL:         time.Hour,
	}
	if err := e.WriteRole(ctx, "app", role); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	return e, p
}

// TestResetReinitializesFromStorage: after Reset (a seal), the engine closes its
// connection and, on next use, initializes from the config storage holds now —
// not the one this process last saw, which another replica may have replaced.
func TestResetReinitializesFromStorage(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	p := &mockPlugin{}
	e := New(mem, "database", p)
	if err := e.Configure(ctx, "mariadb://old"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := e.WriteRole(ctx, "app", Role{
		CreationStatements: []string{"CREATE USER '{{username}}';"}, DefaultTTL: time.Hour,
	}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}

	// Another writer replaces the config behind this engine's back.
	other := New(mem, "database", &mockPlugin{})
	if err := other.Configure(ctx, "mariadb://new"); err != nil {
		t.Fatalf("other Configure: %v", err)
	}

	e.Reset()
	if p.closes != 1 {
		t.Fatalf("Reset closed the plugin %d times, want 1", p.closes)
	}
	if _, err := e.GenerateCredentials(ctx, "app", "test"); err != nil {
		t.Fatalf("GenerateCredentials after Reset: %v", err)
	}
	if p.initURL != "mariadb://new" {
		t.Fatalf("plugin re-initialized with %q, want the stored mariadb://new", p.initURL)
	}

	e.Reset()
	e.Reset() // idempotent: nothing open to close the second time
	if p.closes != 2 {
		t.Fatalf("closes = %d after a second and third Reset, want 2", p.closes)
	}
}

func TestConfigureInitializesPlugin(t *testing.T) {
	e, p := newEngine(t)
	if err := e.Configure(context.Background(), "mariadb://x"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.initURL != "mariadb://x" {
		t.Fatalf("plugin not initialized with URL, got %q", p.initURL)
	}
	if ok, _ := e.Configured(context.Background()); !ok {
		t.Fatal("Configured() = false after Configure")
	}
}

func TestGenerateBeforeConfigure(t *testing.T) {
	e, _ := newEngine(t)
	if _, err := e.GenerateCredentials(context.Background(), "app", ""); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestGenerateCredentials(t *testing.T) {
	ctx := context.Background()
	e, p := configured(t)

	cred, err := e.GenerateCredentials(ctx, "app", "")
	if err != nil {
		t.Fatalf("GenerateCredentials: %v", err)
	}
	if cred.Username == "" || cred.Password == "" || cred.LeaseID == "" {
		t.Fatalf("incomplete credential: %+v", cred)
	}
	if cred.TTL != time.Hour {
		t.Fatalf("ttl = %v, want 1h", cred.TTL)
	}
	// The plugin was asked to create exactly this user with the role's statements.
	if len(p.created) != 1 {
		t.Fatalf("plugin CreateUser calls = %d, want 1", len(p.created))
	}
	if p.created[0].Username != cred.Username {
		t.Fatalf("created username %q != credential username %q", p.created[0].Username, cred.Username)
	}
	if len(p.created[0].CreationStatements) != 1 {
		t.Fatalf("creation statements not passed to plugin")
	}
}

func TestGenerateUnknownRole(t *testing.T) {
	ctx := context.Background()
	e, _ := configured(t)
	if _, err := e.GenerateCredentials(ctx, "missing", ""); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("want ErrRoleNotFound, got %v", err)
	}
}

func TestRevoke(t *testing.T) {
	ctx := context.Background()
	e, p := configured(t)

	cred, _ := e.GenerateCredentials(ctx, "app", "")
	if err := e.Revoke(ctx, cred.LeaseID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if len(p.revoked) != 1 || p.revoked[0] != cred.Username {
		t.Fatalf("revoked = %v, want [%s]", p.revoked, cred.Username)
	}
	// Lease is gone.
	if err := e.Revoke(ctx, cred.LeaseID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("second Revoke: want ErrLeaseNotFound, got %v", err)
	}
}

// TestRevokeExpired is the heart of the lease lifecycle: only leases past their
// expiration are revoked.
func TestRevokeExpired(t *testing.T) {
	ctx := context.Background()
	e, p := configured(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return base }

	cred, _ := e.GenerateCredentials(ctx, "app", "") // expires at base+1h

	// Not yet expired.
	if n, err := e.RevokeExpired(ctx); err != nil || n != 0 {
		t.Fatalf("RevokeExpired before expiry = %d, err %v", n, err)
	}

	// Advance past expiry.
	e.now = func() time.Time { return base.Add(2 * time.Hour) }
	if n, err := e.RevokeExpired(ctx); err != nil || n != 1 {
		t.Fatalf("RevokeExpired after expiry = %d, err %v", n, err)
	}
	if len(p.revoked) != 1 || p.revoked[0] != cred.Username {
		t.Fatalf("revoked = %v", p.revoked)
	}
	// Idempotent: nothing left to revoke.
	if n, _ := e.RevokeExpired(ctx); n != 0 {
		t.Fatalf("second sweep revoked %d, want 0", n)
	}
}

func TestCreateUserFailureDoesNotLeaveLease(t *testing.T) {
	ctx := context.Background()
	e, p := configured(t)
	p.createErr = errors.New("connection refused")

	if _, err := e.GenerateCredentials(ctx, "app", ""); err == nil {
		t.Fatal("expected error when plugin CreateUser fails")
	}
	// No lease should have been recorded.
	ids, _ := e.store.List(ctx, "database/lease/")
	if len(ids) != 0 {
		t.Fatalf("lease recorded despite CreateUser failure: %v", ids)
	}
}

func TestRoleCRUD(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t)
	role := Role{CreationStatements: []string{"CREATE USER '{{username}}';"}, DefaultTTL: 30 * time.Minute}

	if err := e.WriteRole(ctx, "readers", role); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	got, err := e.ReadRole(ctx, "readers")
	if err != nil || got.DefaultTTL != 30*time.Minute {
		t.Fatalf("ReadRole = %+v, err %v", got, err)
	}
	names, _ := e.ListRoles(ctx)
	if len(names) != 1 || names[0] != "readers" {
		t.Fatalf("ListRoles = %v", names)
	}
	if err := e.DeleteRole(ctx, "readers"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if _, err := e.ReadRole(ctx, "readers"); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("after delete: want ErrRoleNotFound, got %v", err)
	}
}

func TestInvalidRoleName(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t)
	if err := e.WriteRole(ctx, "bad/name", Role{}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("want ErrInvalidName, got %v", err)
	}
}

func TestRenewExtendsLease(t *testing.T) {
	ctx := context.Background()
	e, _ := configured(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return base }

	cred, _ := e.GenerateCredentials(ctx, "app", "") // expires base+1h (role TTL)
	base2 := base.Add(30 * time.Minute)
	e.now = func() time.Time { return base2 }

	info, err := e.Renew(ctx, cred.LeaseID, 2*time.Hour)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if info.TTL != 2*time.Hour {
		t.Fatalf("renewed TTL = %v, want 2h", info.TTL)
	}
	// Now the sweep at base+90m must NOT revoke it (new expiry is base+2.5h).
	e.now = func() time.Time { return base.Add(90 * time.Minute) }
	if n, _ := e.RevokeExpired(ctx); n != 0 {
		t.Fatalf("renewed lease was swept: %d", n)
	}
}

func TestLookupLease(t *testing.T) {
	ctx := context.Background()
	e, _ := configured(t)
	cred, _ := e.GenerateCredentials(ctx, "app", "tok-1")

	info, err := e.Lookup(ctx, cred.LeaseID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if info.LeaseID != cred.LeaseID || info.Role != "app" || info.Username != cred.Username {
		t.Fatalf("lookup = %+v", info)
	}
	if _, err := e.Lookup(ctx, "nope"); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("lookup missing: want ErrLeaseNotFound, got %v", err)
	}
}

func TestRevokeByTokenCascades(t *testing.T) {
	ctx := context.Background()
	e, p := configured(t)

	// Two creds from token A, one from token B.
	a1, _ := e.GenerateCredentials(ctx, "app", "token-A")
	a2, _ := e.GenerateCredentials(ctx, "app", "token-A")
	b1, _ := e.GenerateCredentials(ctx, "app", "token-B")

	n, err := e.RevokeByToken(ctx, "token-A")
	if err != nil {
		t.Fatalf("RevokeByToken: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked %d, want 2", n)
	}
	// A's leases are gone; B's remains.
	if _, err := e.Lookup(ctx, a1.LeaseID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatal("a1 lease not revoked")
	}
	if _, err := e.Lookup(ctx, a2.LeaseID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatal("a2 lease not revoked")
	}
	if _, err := e.Lookup(ctx, b1.LeaseID); err != nil {
		t.Fatalf("b1 lease should survive: %v", err)
	}
	// The plugin revoked exactly A's two users.
	if len(p.revoked) != 2 {
		t.Fatalf("plugin revoked %d users, want 2", len(p.revoked))
	}
}

func TestRevokeByTokenEmptyIsNoOp(t *testing.T) {
	ctx := context.Background()
	e, _ := configured(t)
	_, _ = e.GenerateCredentials(ctx, "app", "token-A")
	if n, err := e.RevokeByToken(ctx, ""); err != nil || n != 0 {
		t.Fatalf("RevokeByToken(\"\") = %d, err %v", n, err)
	}
}
