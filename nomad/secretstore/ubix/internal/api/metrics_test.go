package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func TestMetricsBuildInfoVersion(t *testing.T) {
	// build_info must report the version set via WithVersion (regression: the
	// gauge used to be registered before options were applied, so it read "").
	h := NewHandler(core.New(storage.NewMemoryBackend()), WithVersion("9.9.9-test"))
	do(t, h, "GET", "/v1/sys/health", "")
	body := do(t, h, "GET", "/v1/sys/metrics", "").Body.String()
	if !strings.Contains(body, `ubixvault_build_info{version="9.9.9-test"} 1`) {
		t.Fatalf("build_info did not reflect the version:\n%s", body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newTestHandler()

	// Make a prior request so a request count exists (the counter is recorded
	// after the response is written, so it appears on the *next* scrape).
	do(t, h, "GET", "/v1/sys/health", "")

	// Before init: initialized=0, sealed=1.
	rec := do(t, h, "GET", "/v1/sys/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/sys/metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"ubixvault_build_info",
		"ubixvault_initialized 0",
		"ubixvault_sealed 1",
		"ubixvault_uptime_seconds",
		"ubixvault_http_requests_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}

func TestMetricsReflectsUnsealed(t *testing.T) {
	h, _ := unsealedHandler(t)
	body := do(t, h, "GET", "/v1/sys/metrics", "").Body.String()
	if !strings.Contains(body, "ubixvault_initialized 1") || !strings.Contains(body, "ubixvault_sealed 0") {
		t.Fatalf("unsealed metrics wrong:\n%s", body)
	}
}
