package seal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticKEKRoundTrip(t *testing.T) {
	ctx := context.Background()
	kek := make([]byte, kekSize)
	for i := range kek {
		kek[i] = byte(i)
	}
	s := NewStaticKEK(kek)
	if s.Type() != "auto" {
		t.Fatalf("Type = %q, want auto", s.Type())
	}

	master := []byte("this-is-a-32-byte-master-key----")
	wrapped, err := s.Wrap(ctx, master)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	got, err := s.Unwrap(ctx, wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if string(got) != string(master) {
		t.Fatalf("round-trip mismatch: %q", got)
	}

	// A different KEK must fail authentication.
	other := NewStaticKEK(make([]byte, kekSize))
	if _, err := other.Unwrap(ctx, wrapped); err == nil {
		t.Fatal("Unwrap with the wrong KEK should fail")
	}
}

func TestStaticKEKRejectsBadKeyLength(t *testing.T) {
	if _, err := NewStaticKEK([]byte("short")).Wrap(context.Background(), []byte("x")); err == nil {
		t.Fatal("Wrap with a non-32-byte KEK should fail")
	}
}

// transitMock is a minimal Vault-compatible transit engine: encrypt prefixes
// "ct:" to the (base64) plaintext, decrypt strips it back.
func transitMock(t *testing.T, wantToken string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/transit/encrypt/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != wantToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var in struct {
			Plaintext string `json:"plaintext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ciphertext": "ct:" + in.Plaintext}})
	})
	mux.HandleFunc("POST /v1/transit/decrypt/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ciphertext string `json:"ciphertext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"plaintext": strings.TrimPrefix(in.Ciphertext, "ct:")}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestTransitRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv := transitMock(t, "root-token")
	s, err := NewTransit(TransitConfig{Address: srv.URL, Token: "root-token", Key: "unseal"})
	if err != nil {
		t.Fatalf("NewTransit: %v", err)
	}
	if s.Type() != "transit" {
		t.Fatalf("Type = %q, want transit", s.Type())
	}

	master := []byte("another-master-key-goes-in-here!")
	wrapped, err := s.Wrap(ctx, master)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if !strings.HasPrefix(string(wrapped), "ct:") {
		t.Fatalf("wrapped value unexpected: %q", wrapped)
	}
	got, err := s.Unwrap(ctx, wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if string(got) != string(master) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestTransitPropagatesRemoteError(t *testing.T) {
	// A wrong token makes the mock return 403.
	srv := transitMock(t, "right-token")
	s, _ := NewTransit(TransitConfig{Address: srv.URL, Token: "wrong-token", Key: "unseal"})
	if _, err := s.Wrap(context.Background(), []byte("x")); err == nil {
		t.Fatal("Wrap should fail when the remote returns an error status")
	}
}

func TestTransitRequiresConfig(t *testing.T) {
	if _, err := NewTransit(TransitConfig{Address: "https://x"}); err == nil {
		t.Fatal("NewTransit should require token and key")
	}
}
