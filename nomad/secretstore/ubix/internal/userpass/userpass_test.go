package userpass

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

func newMethod(t *testing.T) *Method {
	t.Helper()
	mem := storage.NewMemoryBackend()
	return New(mem, token.NewStore(mem), "auth/userpass")
}

func TestLoginFlow(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)

	if err := m.WriteUser(ctx, "alice", "correct horse", []string{"readers"}, time.Hour, 0); err != nil {
		t.Fatalf("WriteUser: %v", err)
	}
	tok, err := m.Login(ctx, "alice", "correct horse")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(tok.Policies) != 1 || tok.Policies[0] != "readers" {
		t.Fatalf("token policies = %v", tok.Policies)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("token should expire (TokenTTL set)")
	}

	// Wrong password and unknown user both deny.
	if _, err := m.Login(ctx, "alice", "wrong"); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong password: want ErrDenied, got %v", err)
	}
	if _, err := m.Login(ctx, "nobody", "whatever"); !errors.Is(err, ErrDenied) {
		t.Fatalf("unknown user: want ErrDenied, got %v", err)
	}
}

func TestReadUserHidesSecret(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	const pw = "distinctive-Passw0rd-9f3a2b"
	_ = m.WriteUser(ctx, "bob", pw, []string{"p"}, 0, 0)
	info, err := m.ReadUser(ctx, "bob")
	if err != nil {
		t.Fatalf("ReadUser: %v", err)
	}
	if len(info.Policies) != 1 || info.Policies[0] != "p" {
		t.Fatalf("policies = %v", info.Policies)
	}
	// The stored record must not keep the password in the clear.
	entry, _ := m.store.Get(ctx, m.userKey("bob"))
	if entry != nil && strings.Contains(string(entry.Value), pw) {
		t.Fatal("stored user record contains the plaintext password")
	}
}

func TestWriteUserValidation(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	if err := m.WriteUser(ctx, "x", "", []string{"p"}, 0, 0); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty password: want ErrInvalidConfig, got %v", err)
	}
	if err := m.WriteUser(ctx, "x", "pw", nil, 0, 0); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("no policies: want ErrInvalidConfig, got %v", err)
	}
	if err := m.WriteUser(ctx, "bad/name", "pw", []string{"p"}, 0, 0); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("bad name: want ErrInvalidName, got %v", err)
	}
}

func TestPasswordUpdate(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	_ = m.WriteUser(ctx, "carol", "old", []string{"p"}, 0, 0)
	_ = m.WriteUser(ctx, "carol", "new", []string{"p"}, 0, 0) // rotate
	if _, err := m.Login(ctx, "carol", "old"); !errors.Is(err, ErrDenied) {
		t.Fatalf("old password should no longer work, got %v", err)
	}
	if _, err := m.Login(ctx, "carol", "new"); err != nil {
		t.Fatalf("new password should work: %v", err)
	}
}
