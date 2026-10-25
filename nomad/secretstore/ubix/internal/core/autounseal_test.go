package core

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newKEK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, masterKeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestAutoUnsealInitLeavesUnsealed(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemoryBackend(), WithAutoUnsealKey(newKEK(t)))

	res, err := c.Initialize(ctx, InitConfig{})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if res.RootToken == "" {
		t.Fatal("no root token")
	}
	if len(res.Keys) != 0 {
		t.Fatalf("auto-unseal init returned %d unseal keys, want 0", len(res.Keys))
	}
	if c.Barrier().Sealed() {
		t.Fatal("auto-unseal vault should be unsealed after init")
	}

	st, _ := c.Status(ctx)
	if st.Type != SealTypeAuto || st.Sealed {
		t.Fatalf("status = %+v, want type=auto sealed=false", st)
	}
}

func TestAutoUnsealAfterReseal(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	kek := newKEK(t)
	c := New(mem, WithAutoUnsealKey(kek))
	if _, err := c.Initialize(ctx, InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Barrier().Put(ctx, &storage.Entry{Key: "secret/x", Value: []byte("v")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	c.Seal()
	if !c.Barrier().Sealed() {
		t.Fatal("not sealed after Seal()")
	}
	if err := c.AutoUnseal(ctx); err != nil {
		t.Fatalf("AutoUnseal: %v", err)
	}
	got, err := c.Barrier().Get(ctx, "secret/x")
	if err != nil || got == nil || string(got.Value) != "v" {
		t.Fatalf("data after auto-unseal: got %+v, err %v", got, err)
	}
}

func TestAutoUnsealAcrossRestart(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	kek := newKEK(t)

	c1 := New(mem, WithAutoUnsealKey(kek))
	if _, err := c1.Initialize(ctx, InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	_ = c1.Barrier().Put(ctx, &storage.Entry{Key: "secret/x", Value: []byte("durable")})

	// "Restart": a new Core over the same backend with the same KEK auto-unseals.
	c2 := New(mem, WithAutoUnsealKey(kek))
	if err := c2.AutoUnseal(ctx); err != nil {
		t.Fatalf("AutoUnseal after restart: %v", err)
	}
	got, err := c2.Barrier().Get(ctx, "secret/x")
	if err != nil || got == nil || string(got.Value) != "durable" {
		t.Fatalf("data after restart: got %+v, err %v", got, err)
	}
}

func TestAutoUnsealWrongKey(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()

	c1 := New(mem, WithAutoUnsealKey(newKEK(t)))
	if _, err := c1.Initialize(ctx, InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	c2 := New(mem, WithAutoUnsealKey(newKEK(t))) // different KEK
	if err := c2.AutoUnseal(ctx); err == nil {
		t.Fatal("AutoUnseal with wrong key should fail")
	}
	if !c2.Barrier().Sealed() {
		t.Fatal("barrier unsealed with the wrong KEK")
	}
}

func TestManualUnsealRejectedInAutoMode(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemoryBackend(), WithAutoUnsealKey(newKEK(t)))
	if _, err := c.Initialize(ctx, InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.Seal()
	if _, err := c.Unseal(ctx, make([]byte, masterKeySize+1)); !errors.Is(err, ErrAutoUnsealShamir) {
		t.Fatalf("manual unseal in auto mode: want ErrAutoUnsealShamir, got %v", err)
	}
}

func TestAutoUnsealNotConfigured(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemoryBackend()) // Shamir mode
	if err := c.AutoUnseal(ctx); !errors.Is(err, ErrAutoUnsealNotConfigured) {
		t.Fatalf("want ErrAutoUnsealNotConfigured, got %v", err)
	}
}

func TestShamirInitRecordsType(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemoryBackend())
	if _, err := c.Initialize(ctx, InitConfig{SecretShares: 3, SecretThreshold: 2}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	st, _ := c.Status(ctx)
	if st.Type != SealTypeShamir {
		t.Fatalf("shamir status type = %q, want shamir", st.Type)
	}
}
