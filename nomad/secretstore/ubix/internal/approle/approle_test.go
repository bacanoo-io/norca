package approle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

func newMethod(t *testing.T) *Method {
	t.Helper()
	mem := storage.NewMemoryBackend()
	return New(mem, token.NewStore(mem), "auth/approle")
}

func TestLoginFlow(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)

	if err := m.WriteRole(ctx, "ci", Role{Policies: []string{"readers"}}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	roleID, err := m.RoleID(ctx, "ci")
	if err != nil || roleID == "" {
		t.Fatalf("RoleID: %q, %v", roleID, err)
	}
	secretID, err := m.GenerateSecretID(ctx, "ci")
	if err != nil || secretID == "" {
		t.Fatalf("GenerateSecretID: %q, %v", secretID, err)
	}

	tok, err := m.Login(ctx, roleID, secretID)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(tok.Policies) != 1 || tok.Policies[0] != "readers" {
		t.Fatalf("token policies = %v, want [readers]", tok.Policies)
	}

	// Wrong secret_id and wrong role_id both deny, indistinguishably.
	if _, err := m.Login(ctx, roleID, "wrong"); !errors.Is(err, ErrDenied) {
		t.Fatalf("bad secret_id: want ErrDenied, got %v", err)
	}
	if _, err := m.Login(ctx, "wrongid", secretID); !errors.Is(err, ErrDenied) {
		t.Fatalf("bad role_id: want ErrDenied, got %v", err)
	}
}

func TestRoleIDStableAcrossUpdates(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	if err := m.WriteRole(ctx, "r", Role{Policies: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	first, _ := m.RoleID(ctx, "r")
	if err := m.WriteRole(ctx, "r", Role{Policies: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if again, _ := m.RoleID(ctx, "r"); again != first {
		t.Fatalf("role_id changed on update: %q -> %q", first, again)
	}
}

func TestPolicyRequired(t *testing.T) {
	if err := newMethod(t).WriteRole(context.Background(), "r", Role{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig for a role with no policies, got %v", err)
	}
}

func TestTokenTTLApplied(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	_ = m.WriteRole(ctx, "r", Role{Policies: []string{"p"}, TokenTTL: time.Hour})
	roleID, _ := m.RoleID(ctx, "r")
	secretID, _ := m.GenerateSecretID(ctx, "r")
	tok, err := m.Login(ctx, roleID, secretID)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("token from a role with TokenTTL should expire")
	}
}

func TestExpiredSecretIDDenied(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	// A sub-second TTL truncates to the current second, so the secret_id is
	// already expired by the time Login checks it.
	_ = m.WriteRole(ctx, "r", Role{Policies: []string{"p"}, SecretIDTTL: time.Nanosecond})
	roleID, _ := m.RoleID(ctx, "r")
	secretID, _ := m.GenerateSecretID(ctx, "r")
	if _, err := m.Login(ctx, roleID, secretID); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired secret_id: want ErrDenied, got %v", err)
	}
}
