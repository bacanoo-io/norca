package api

import (
	"net/http"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/ratelimit"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func TestRateLimitThrottlesAPI(t *testing.T) {
	// burst 2, so the 3rd API request from the same client is rejected.
	h := NewHandler(core.New(storage.NewMemoryBackend()), WithRateLimit(ratelimit.New(1, 2)))

	got := []int{}
	for i := 0; i < 3; i++ {
		got = append(got, do(t, h, "GET", "/v1/sys/seal-status", "").Code)
	}
	if got[0] == http.StatusTooManyRequests || got[1] == http.StatusTooManyRequests {
		t.Fatalf("first two requests should be allowed, got %v", got)
	}
	if got[2] != http.StatusTooManyRequests {
		t.Fatalf("3rd request = %d, want 429", got[2])
	}
	if ra := do(t, h, "GET", "/v1/sys/seal-status", "").Header().Get("Retry-After"); ra == "" {
		t.Error("429 response should carry a Retry-After header")
	}
}

func TestRateLimitExemptsPublicEndpoints(t *testing.T) {
	h := NewHandler(core.New(storage.NewMemoryBackend()), WithRateLimit(ratelimit.New(1, 1)))
	// Health and metrics must never be throttled (probes/scrapers hit them often).
	for i := 0; i < 20; i++ {
		if rec := do(t, h, "GET", "/v1/sys/health", ""); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("health throttled on request %d", i+1)
		}
		if rec := do(t, h, "GET", "/v1/sys/metrics", ""); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("metrics throttled on request %d", i+1)
		}
	}
}
