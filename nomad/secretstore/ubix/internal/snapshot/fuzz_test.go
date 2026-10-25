package snapshot

import (
	"bytes"
	"context"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// FuzzRestore feeds arbitrary bytes to the snapshot reader. Restore reads an
// operator-supplied file during disaster recovery, so malformed, truncated, or
// hostile input must produce an error, never a panic — and never a partially
// corrupt backend that panics on later use.
func FuzzRestore(f *testing.F) {
	seeds := []string{
		"",
		"ubixvault-snapshot v1\n",
		"ubixvault-snapshot v1\n{\"key\":\"a/b\",\"value\":\"AQID\"}\n",
		"ubixvault-snapshot v2\n",               // wrong version
		"wrong-header\n{\"key\":\"a\"}\n",       // bad header
		"ubixvault-snapshot v1\n{not json}\n",   // malformed line
		"ubixvault-snapshot v1\n{\"key\":\"\"}", // invalid (empty) key, truncated
		"ubixvault-snapshot v1\n{\"key\":\"../escape\",\"value\":\"AA\"}\n",
		"ubixvault-snapshot v1\n{\"key\":\"a\",\"value\":\"@@notbase64@@\"}\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		b := storage.NewMemoryBackend()
		// Must not panic. Success or error are both acceptable outcomes.
		if err := Restore(context.Background(), b, bytes.NewReader(data)); err != nil {
			return
		}
		// If it claimed success, the backend must be usable (a List must not panic).
		if _, err := b.List(context.Background(), ""); err != nil {
			t.Fatalf("List after successful Restore: %v", err)
		}
	})
}
