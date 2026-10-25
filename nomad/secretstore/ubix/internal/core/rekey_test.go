package core

import (
	"context"
	"errors"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// rekeyComplete drives a full rekey from oldKeys (supplying `threshold` of them)
// to a new newShares/newThreshold configuration, returning the new shares.
func rekeyComplete(t *testing.T, c *Core, oldKeys [][]byte, oldThreshold, newShares, newThreshold int) [][]byte {
	t.Helper()
	ctx := context.Background()
	st, err := c.RekeyInit(ctx, newShares, newThreshold)
	if err != nil {
		t.Fatalf("RekeyInit: %v", err)
	}
	for i := 0; i < oldThreshold; i++ {
		st, err = c.RekeyUpdate(ctx, st.Nonce, oldKeys[i])
		if err != nil {
			t.Fatalf("RekeyUpdate %d: %v", i, err)
		}
	}
	if !st.Complete {
		t.Fatal("rekey did not complete at threshold")
	}
	if len(st.Keys) != newShares {
		t.Fatalf("got %d new keys, want %d", len(st.Keys), newShares)
	}
	return st.Keys
}

func TestRekeyRotatesSharesAndKeepsData(t *testing.T) {
	ctx := context.Background()
	c, oldKeys := initCore(t, 3, 2)
	for i := 0; i < 2; i++ {
		if _, err := c.Unseal(ctx, oldKeys[i]); err != nil {
			t.Fatalf("Unseal %d: %v", i, err)
		}
	}
	if err := c.Barrier().Put(ctx, &storage.Entry{Key: "app/x", Value: []byte("v")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	newKeys := rekeyComplete(t, c, oldKeys, 2, 5, 3)

	// The barrier stayed unsealed, so data is readable throughout.
	got, err := c.Barrier().Get(ctx, "app/x")
	if err != nil || string(got.Value) != "v" {
		t.Fatalf("data after rekey = %v, %v; want v", got, err)
	}

	// Seal, then the OLD shares must no longer unseal. The new threshold is 3, so
	// supply all three old shares; the final one triggers reconstruction, which
	// yields the old master key and no longer authenticates the rewrapped keyring.
	c.Seal()
	_, _ = c.Unseal(ctx, oldKeys[0])
	_, _ = c.Unseal(ctx, oldKeys[1])
	if _, err := c.Unseal(ctx, oldKeys[2]); !errors.Is(err, ErrUnsealFailed) {
		t.Fatalf("unseal with old shares = %v, want ErrUnsealFailed", err)
	}

	// The NEW shares unseal, and the new config is 5/3.
	var st *SealStatus
	for i := 0; i < 3; i++ {
		if st, err = c.Unseal(ctx, newKeys[i]); err != nil {
			t.Fatalf("unseal with new share %d: %v", i, err)
		}
	}
	if st.Sealed {
		t.Fatal("vault still sealed after supplying new threshold")
	}
	if st.Shares != 5 || st.Threshold != 3 {
		t.Fatalf("new config = %d/%d, want 5/3", st.Shares, st.Threshold)
	}
}

func TestRekeyRequiresUnsealed(t *testing.T) {
	c, _ := initCore(t, 3, 2) // left sealed
	if _, err := c.RekeyInit(context.Background(), 5, 3); !errors.Is(err, ErrRekeySealed) {
		t.Fatalf("RekeyInit while sealed = %v, want ErrRekeySealed", err)
	}
}

func TestRekeyInvalidConfig(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 3, 2)
	for i := 0; i < 2; i++ {
		_, _ = c.Unseal(ctx, keys[i])
	}
	if _, err := c.RekeyInit(ctx, 1, 2); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("RekeyInit(1,2) = %v, want ErrInvalidConfig", err)
	}
}

func TestRekeyWrongNonce(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 3, 2)
	for i := 0; i < 2; i++ {
		_, _ = c.Unseal(ctx, keys[i])
	}
	if _, err := c.RekeyInit(ctx, 5, 3); err != nil {
		t.Fatalf("RekeyInit: %v", err)
	}
	if _, err := c.RekeyUpdate(ctx, "bogus-nonce", keys[0]); !errors.Is(err, ErrRekeyNonce) {
		t.Fatalf("RekeyUpdate wrong nonce = %v, want ErrRekeyNonce", err)
	}
}

func TestRekeyCancel(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 3, 2)
	for i := 0; i < 2; i++ {
		_, _ = c.Unseal(ctx, keys[i])
	}
	st, err := c.RekeyInit(ctx, 5, 3)
	if err != nil {
		t.Fatalf("RekeyInit: %v", err)
	}
	c.RekeyCancel()
	if _, err := c.RekeyUpdate(ctx, st.Nonce, keys[0]); !errors.Is(err, ErrRekeyNotStarted) {
		t.Fatalf("RekeyUpdate after cancel = %v, want ErrRekeyNotStarted", err)
	}
}

func TestRekeyWrongSharesFail(t *testing.T) {
	ctx := context.Background()
	c, keys := initCore(t, 3, 2)
	for i := 0; i < 2; i++ {
		_, _ = c.Unseal(ctx, keys[i])
	}
	st, err := c.RekeyInit(ctx, 5, 3)
	if err != nil {
		t.Fatalf("RekeyInit: %v", err)
	}
	// The rekey quorum is the current threshold (2). Corrupt one real share's
	// y-value so the two shares reconstruct the wrong key (structurally valid,
	// distinct x-coordinates — it just isn't the master key).
	bad := append([]byte(nil), keys[1]...)
	bad[1] ^= 0xFF
	_, _ = c.RekeyUpdate(ctx, st.Nonce, keys[0])
	if _, err := c.RekeyUpdate(ctx, st.Nonce, bad); !errors.Is(err, ErrUnsealFailed) {
		t.Fatalf("RekeyUpdate with wrong shares = %v, want ErrUnsealFailed", err)
	}
}
