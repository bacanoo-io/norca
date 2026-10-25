package client

import (
	"context"
	"testing"
)

func TestRekeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv := testServer(t)
	c := New(srv.URL, "")

	// Initialize 3/2 and unseal.
	res, err := c.Init(ctx, 3, 2)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := c.Unseal(ctx, res.Keys[0]); err != nil {
		t.Fatalf("Unseal 0: %v", err)
	}
	if _, err := c.Unseal(ctx, res.Keys[1]); err != nil {
		t.Fatalf("Unseal 1: %v", err)
	}

	// Rekey to 5/3.
	st, err := c.RekeyInit(ctx, 5, 3)
	if err != nil {
		t.Fatalf("RekeyInit: %v", err)
	}
	if !st.Started || st.Nonce == "" || st.Required != 2 {
		t.Fatalf("rekey init = %+v", st)
	}

	// Status shows it in progress.
	if s, _ := c.RekeyStatus(ctx); !s.Started || s.NewShares != 5 {
		t.Fatalf("rekey status = %+v", s)
	}

	// Feed the two current shares; the second completes with 5 new keys.
	if _, err := c.RekeyUpdate(ctx, st.Nonce, res.Keys[0]); err != nil {
		t.Fatalf("RekeyUpdate 0: %v", err)
	}
	done, err := c.RekeyUpdate(ctx, st.Nonce, res.Keys[1])
	if err != nil {
		t.Fatalf("RekeyUpdate 1: %v", err)
	}
	if !done.Complete || len(done.Keys) != 5 {
		t.Fatalf("rekey completion = %+v", done)
	}

	// The new shares unseal after a re-seal (proving the rotation took effect).
	authed := New(srv.URL, res.RootToken)
	if err := authed.Seal(ctx); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Unseal(ctx, done.Keys[i]); err != nil {
			t.Fatalf("Unseal with new key %d: %v", i, err)
		}
	}
	if s, _ := c.SealStatus(ctx); s.Sealed || s.Shares != 5 || s.Threshold != 3 {
		t.Fatalf("post-rekey status = %+v", s)
	}
}

func TestRekeyCancel(t *testing.T) {
	ctx := context.Background()
	srv := testServer(t)
	c := New(srv.URL, "")

	res, _ := c.Init(ctx, 2, 2)
	_, _ = c.Unseal(ctx, res.Keys[0])
	_, _ = c.Unseal(ctx, res.Keys[1])

	if _, err := c.RekeyInit(ctx, 3, 2); err != nil {
		t.Fatalf("RekeyInit: %v", err)
	}
	if err := c.RekeyCancel(ctx); err != nil {
		t.Fatalf("RekeyCancel: %v", err)
	}
	if s, _ := c.RekeyStatus(ctx); s.Started {
		t.Fatalf("expected no attempt after cancel, got %+v", s)
	}
}
