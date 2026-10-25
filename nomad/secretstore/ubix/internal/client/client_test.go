package client

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/api"
	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func TestTLSOptions(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewTLSServer(api.NewHandler(core.New(storage.NewMemoryBackend())))
	t.Cleanup(srv.Close)

	// A default client must reject the server's self-signed certificate.
	if _, err := New(srv.URL, "").SealStatus(ctx); err == nil {
		t.Fatal("expected TLS verification failure against self-signed server, got nil")
	}

	// -tls-skip-verify reaches it.
	if _, err := New(srv.URL, "", WithTLSSkipVerify(true)).SealStatus(ctx); err != nil {
		t.Fatalf("WithTLSSkipVerify: %v", err)
	}

	// Trusting the server's CA reaches it without skipping verification.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	if _, err := New(srv.URL, "", WithRootCAs(pool)).SealStatus(ctx); err != nil {
		t.Fatalf("WithRootCAs: %v", err)
	}
}

// testServer spins up a real handler over an in-memory backend.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(api.NewHandler(core.New(storage.NewMemoryBackend())))
	t.Cleanup(srv.Close)
	return srv
}

func TestInitUnsealSealStatus(t *testing.T) {
	ctx := context.Background()
	srv := testServer(t)
	c := New(srv.URL, "")

	// Not initialized yet.
	st, err := c.SealStatus(ctx)
	if err != nil {
		t.Fatalf("SealStatus: %v", err)
	}
	if st.Initialized || !st.Sealed {
		t.Fatalf("pre-init status = %+v", st)
	}

	// Initialize.
	res, err := c.Init(ctx, 3, 2)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if len(res.Keys) != 3 || res.RootToken == "" {
		t.Fatalf("init result = %+v", res)
	}

	// Unseal with two shares.
	if st, _ = c.Unseal(ctx, res.Keys[0]); !st.Sealed || st.Progress != 1 {
		t.Fatalf("after 1 share: %+v", st)
	}
	if st, _ = c.Unseal(ctx, res.Keys[1]); st.Sealed {
		t.Fatalf("still sealed after threshold: %+v", st)
	}

	// Status reflects unsealed.
	st, _ = c.SealStatus(ctx)
	if st.Sealed || st.Threshold != 2 || st.Shares != 3 {
		t.Fatalf("post-unseal status = %+v", st)
	}
}

func TestSealRequiresToken(t *testing.T) {
	ctx := context.Background()
	srv := testServer(t)

	// Initialize + unseal via a no-token client.
	anon := New(srv.URL, "")
	res, _ := anon.Init(ctx, 2, 2)
	_, _ = anon.Unseal(ctx, res.Keys[0])
	_, _ = anon.Unseal(ctx, res.Keys[1])

	// Seal without a token fails.
	if err := anon.Seal(ctx); err == nil {
		t.Fatal("seal without token should fail")
	}
	var apiErr *APIError
	if !errors.As(anon.Seal(ctx), &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("expected 401 APIError, got %v", anon.Seal(ctx))
	}

	// Seal with the root token succeeds.
	authed := New(srv.URL, res.RootToken)
	if err := authed.Seal(ctx); err != nil {
		t.Fatalf("seal with token: %v", err)
	}
	if st, _ := authed.SealStatus(ctx); !st.Sealed {
		t.Fatal("not sealed after Seal()")
	}
}

func TestAPIErrorMessage(t *testing.T) {
	ctx := context.Background()
	srv := testServer(t)
	c := New(srv.URL, "")

	// Invalid init config -> 400 with an error message.
	_, err := c.Init(ctx, 1, 1)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.StatusCode != 400 || len(apiErr.Errors) == 0 {
		t.Fatalf("APIError = %+v", apiErr)
	}
}
