package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// downBackend simulates storage that is hard-down: every operation reports the
// unavailable condition the retry layer surfaces once retries are exhausted.
type downBackend struct{}

func (downBackend) Get(context.Context, string) (*storage.Entry, error) {
	return nil, storage.ErrUnavailable
}
func (downBackend) Put(context.Context, *storage.Entry) error { return storage.ErrUnavailable }
func (downBackend) Delete(context.Context, string) error      { return storage.ErrUnavailable }
func (downBackend) List(context.Context, string) ([]string, error) {
	return nil, storage.ErrUnavailable
}

// errBackend fails every operation with a non-transient (permanent) error.
type errBackend struct{}

func (errBackend) Get(context.Context, string) (*storage.Entry, error) {
	return nil, errors.New("permanent boom")
}
func (errBackend) Put(context.Context, *storage.Entry) error { return errors.New("permanent boom") }
func (errBackend) Delete(context.Context, string) error      { return errors.New("permanent boom") }
func (errBackend) List(context.Context, string) ([]string, error) {
	return nil, errors.New("permanent boom")
}

func TestHealthUninitialized(t *testing.T) {
	h := newTestHandler()
	rec := do(t, h, "GET", "/v1/sys/health", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("uninitialized health = %d, want 501", rec.Code)
	}
	body := decode[healthResponse](t, rec)
	if body.Initialized || !body.Sealed {
		t.Fatalf("health body = %+v", body)
	}
}

func TestHealthSealed(t *testing.T) {
	h := newTestHandler()
	do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`)
	// Initialized but sealed.
	rec := do(t, h, "GET", "/v1/sys/health", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("sealed health = %d, want 503", rec.Code)
	}
	if body := decode[healthResponse](t, rec); !body.Initialized || !body.Sealed {
		t.Fatalf("health body = %+v", body)
	}
}

func TestHealthReady(t *testing.T) {
	h, _ := unsealedHandler(t)
	rec := do(t, h, "GET", "/v1/sys/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ready health = %d, want 200", rec.Code)
	}
	body := decode[healthResponse](t, rec)
	if !body.Initialized || body.Sealed {
		t.Fatalf("health body = %+v", body)
	}
	if body.ServerTimeUTC == 0 {
		t.Fatal("server_time_utc not set")
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	h, _ := unsealedHandler(t)
	// No X-Vault-Token — health must still respond 200.
	if rec := do(t, h, "GET", "/v1/sys/health", ""); rec.Code != http.StatusOK {
		t.Fatalf("health without token = %d, want 200", rec.Code)
	}
}

func TestLivezAlwaysOK(t *testing.T) {
	// Uninitialized: health is 501, but livez must still be 200 — a liveness
	// probe must not crash-loop a not-yet-initialized (or sealed) vault.
	h := newTestHandler()
	if rec := do(t, h, "GET", "/v1/sys/livez", ""); rec.Code != http.StatusOK {
		t.Fatalf("livez (uninitialized) = %d, want 200", rec.Code)
	}
	// Unsealed: still 200.
	hu, _ := unsealedHandler(t)
	if rec := do(t, hu, "GET", "/v1/sys/livez", ""); rec.Code != http.StatusOK {
		t.Fatalf("livez (unsealed) = %d, want 200", rec.Code)
	}
}

// TestLivezSurvivesStorageDown is the crux of the crash-loop fix: with storage
// hard-down, livez must still be 200 so the liveness probe never SIGKILLs the
// process (every restart is also an unseal cycle).
func TestLivezSurvivesStorageDown(t *testing.T) {
	h := NewHandler(core.New(downBackend{}))
	if rec := do(t, h, "GET", "/v1/sys/livez", ""); rec.Code != http.StatusOK {
		t.Fatalf("livez with storage hard-down = %d, want 200", rec.Code)
	}
}

// TestHealthReturns503WhenStorageDown confirms readiness fails cleanly (503,
// not 500) when storage is unavailable, so traffic drains and recovers on its
// own — without the process dying.
func TestHealthReturns503WhenStorageDown(t *testing.T) {
	h := NewHandler(core.New(downBackend{}))
	if rec := do(t, h, "GET", "/v1/sys/health", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health with storage down = %d, want 503", rec.Code)
	}
}

// TestHealthReturns500OnPermanentError confirms a genuine (non-transient) error
// still surfaces as 500 — the ErrUnavailable→503 mapping does not swallow real bugs.
func TestHealthReturns500OnPermanentError(t *testing.T) {
	h := NewHandler(core.New(errBackend{}))
	if rec := do(t, h, "GET", "/v1/sys/health", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("health with permanent error = %d, want 500", rec.Code)
	}
}
