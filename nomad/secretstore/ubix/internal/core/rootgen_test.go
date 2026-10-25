package core

import (
	"context"
	"errors"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/shamir"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// unsealedShamirCore returns an initialized, unsealed Shamir core and its keys.
func unsealedShamirCore(t *testing.T) (*Core, [][]byte) {
	t.Helper()
	ctx := context.Background()
	c := New(storage.NewMemoryBackend())
	res, err := c.Initialize(ctx, InitConfig{SecretShares: 3, SecretThreshold: 2})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.Unseal(ctx, res.Keys[0]); err != nil {
		t.Fatalf("Unseal 0: %v", err)
	}
	if _, err := c.Unseal(ctx, res.Keys[1]); err != nil {
		t.Fatalf("Unseal 1: %v", err)
	}
	return c, res.Keys
}

func TestGenerateRootSuccess(t *testing.T) {
	ctx := context.Background()
	c, keys := unsealedShamirCore(t)

	st, err := c.GenerateRootInit(ctx)
	if err != nil {
		t.Fatalf("GenerateRootInit: %v", err)
	}
	if !st.Started || st.Nonce == "" || st.Required != 2 {
		t.Fatalf("init status = %+v", st)
	}

	if s, _ := c.GenerateRootUpdate(ctx, st.Nonce, keys[0]); s.Complete {
		t.Fatalf("completed after 1 share: %+v", s)
	}
	done, err := c.GenerateRootUpdate(ctx, st.Nonce, keys[2])
	if err != nil {
		t.Fatalf("GenerateRootUpdate: %v", err)
	}
	if !done.Complete || done.RootToken == "" {
		t.Fatalf("final status = %+v", done)
	}

	// The new token is a valid root token.
	tok, err := c.Tokens().Lookup(ctx, done.RootToken)
	if err != nil || !tok.IsRoot() {
		t.Fatalf("new root token invalid: tok=%+v err=%v", tok, err)
	}
}

func TestGenerateRootWrongNonce(t *testing.T) {
	ctx := context.Background()
	c, keys := unsealedShamirCore(t)
	if _, err := c.GenerateRootInit(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := c.GenerateRootUpdate(ctx, "bogus-nonce", keys[0]); !errors.Is(err, ErrRootGenNonce) {
		t.Fatalf("want ErrRootGenNonce, got %v", err)
	}
}

func TestGenerateRootWrongShares(t *testing.T) {
	ctx := context.Background()
	c, keys := unsealedShamirCore(t)
	st, _ := c.GenerateRootInit(ctx)

	bad := append([]byte(nil), keys[1]...)
	bad[0] ^= 0xFF // corrupt a y-value so the reconstructed key is wrong

	_, _ = c.GenerateRootUpdate(ctx, st.Nonce, keys[0])
	if _, err := c.GenerateRootUpdate(ctx, st.Nonce, bad); !errors.Is(err, ErrUnsealFailed) {
		t.Fatalf("want ErrUnsealFailed, got %v", err)
	}
	// The attempt is reset after failure.
	if s, _ := c.GenerateRootStatus(ctx); s.Started {
		t.Fatalf("attempt not reset after failure: %+v", s)
	}
}

func TestGenerateRootNotStarted(t *testing.T) {
	ctx := context.Background()
	c, keys := unsealedShamirCore(t)
	if _, err := c.GenerateRootUpdate(ctx, "n", keys[0]); !errors.Is(err, ErrRootGenNotStarted) {
		t.Fatalf("want ErrRootGenNotStarted, got %v", err)
	}
}

func TestGenerateRootSealed(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemoryBackend())
	if _, err := c.Initialize(ctx, InitConfig{SecretShares: 3, SecretThreshold: 2}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// Not unsealed.
	if _, err := c.GenerateRootInit(ctx); !errors.Is(err, ErrRootGenSealed) {
		t.Fatalf("want ErrRootGenSealed, got %v", err)
	}
}

func TestGenerateRootAutoModeWithRecoveryKeys(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemoryBackend(), WithAutoUnsealKey(newKEK(t)))
	res, err := c.Initialize(ctx, InitConfig{SecretShares: 3, SecretThreshold: 2})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if len(res.Keys) != 0 {
		t.Fatalf("auto-unseal returned %d unseal keys, want 0", len(res.Keys))
	}
	if len(res.RecoveryKeys) != 3 {
		t.Fatalf("got %d recovery keys, want 3", len(res.RecoveryKeys))
	}

	// Regenerate a root token using a threshold of recovery keys.
	st, err := c.GenerateRootInit(ctx)
	if err != nil {
		t.Fatalf("GenerateRootInit: %v", err)
	}
	if st.Required != 2 {
		t.Fatalf("required = %d, want 2", st.Required)
	}
	var out *RootGenStatus
	for _, share := range res.RecoveryKeys[:2] {
		out, err = c.GenerateRootUpdate(ctx, st.Nonce, share)
		if err != nil {
			t.Fatalf("GenerateRootUpdate: %v", err)
		}
	}
	if !out.Complete || out.RootToken == "" {
		t.Fatalf("root regeneration did not complete: %+v", out)
	}
}

func TestGenerateRootAutoModeRejectsWrongKeys(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemoryBackend(), WithAutoUnsealKey(newKEK(t)))
	if _, err := c.Initialize(ctx, InitConfig{SecretShares: 3, SecretThreshold: 2}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// Shares from an unrelated split must not reconstruct the recovery key.
	bogus, err := shamir.Split(make([]byte, 32), 3, 2)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	st, err := c.GenerateRootInit(ctx)
	if err != nil {
		t.Fatalf("GenerateRootInit: %v", err)
	}
	if _, err := c.GenerateRootUpdate(ctx, st.Nonce, bogus[0]); err != nil {
		t.Fatalf("first share: %v", err)
	}
	if _, err := c.GenerateRootUpdate(ctx, st.Nonce, bogus[1]); !errors.Is(err, ErrUnsealFailed) {
		t.Fatalf("want ErrUnsealFailed for wrong recovery keys, got %v", err)
	}
}

func TestGenerateRootCancel(t *testing.T) {
	ctx := context.Background()
	c, _ := unsealedShamirCore(t)
	if _, err := c.GenerateRootInit(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	c.GenerateRootCancel()
	if s, _ := c.GenerateRootStatus(ctx); s.Started {
		t.Fatalf("attempt still active after cancel: %+v", s)
	}
}
