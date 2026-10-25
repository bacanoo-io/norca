package core

import (
	"context"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func TestOnSealRunsAfterSealOutsideLock(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemoryBackend(), WithAutoUnsealKey(newKEK(t)))
	if _, err := c.Initialize(ctx, InitConfig{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	var calls int
	var sawSealed bool
	c.OnSeal(func() {
		calls++
		// Calling back into the core must not deadlock: hooks run outside c.mu.
		st, err := c.Status(ctx)
		if err != nil {
			t.Errorf("Status from hook: %v", err)
			return
		}
		sawSealed = st.Sealed
	})

	c.Seal()
	if calls != 1 {
		t.Fatalf("hook ran %d times, want 1", calls)
	}
	if !sawSealed {
		t.Fatal("hook ran before the barrier was sealed")
	}
}
