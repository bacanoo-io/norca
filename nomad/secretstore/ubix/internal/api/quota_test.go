package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestQuota_RateLimitCRUD(t *testing.T) {
	h, root := unsealedHandler(t)

	// Create.
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/rate-limit/apicap",
		`{"path":"secret/","rate":50,"burst":100}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body)
	}

	// Read back.
	rec := doAuth(t, h, "GET", "/v1/sys/quotas/rate-limit/apicap", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: code=%d body=%s", rec.Code, rec.Body)
	}
	got := decode[struct {
		Data struct {
			Name, Type, Path string
			Rate, Burst      float64
		}
	}](t, rec)
	d := got.Data
	if d.Name != "apicap" || d.Type != "rate-limit" || d.Path != "secret/" || d.Rate != 50 || d.Burst != 100 {
		t.Errorf("read fields: %+v", d)
	}

	// List.
	rec = doAuth(t, h, "LIST", "/v1/sys/quotas/rate-limit", "", root)
	list := decode[struct{ Data struct{ Keys []string } }](t, rec)
	if len(list.Data.Keys) != 1 || list.Data.Keys[0] != "apicap" {
		t.Errorf("list keys = %v, want [apicap]", list.Data.Keys)
	}

	// Delete → gone.
	if rec := doAuth(t, h, "DELETE", "/v1/sys/quotas/rate-limit/apicap", "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/sys/quotas/rate-limit/apicap", "", root); rec.Code != http.StatusNotFound {
		t.Errorf("read after delete: code=%d, want 404", rec.Code)
	}
}

func TestQuota_RateLimitRequiresAuth(t *testing.T) {
	h, _ := unsealedHandler(t)
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/rate-limit/x", `{"rate":1}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated create: code=%d, want 401", rec.Code)
	}
}

func TestQuota_RateLimitValidation(t *testing.T) {
	h, root := unsealedHandler(t)
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/rate-limit/bad", `{"path":"secret/","rate":0}`, root); rec.Code != http.StatusBadRequest {
		t.Errorf("zero-rate create: code=%d, want 400", rec.Code)
	}
}

// TestQuota_EnforcedInMiddleware proves a path-scoped quota is enforced end to end:
// requests under the quota's path get a 429 once the burst is spent, while a
// request on an unrelated path is unaffected.
func TestQuota_EnforcedInMiddleware(t *testing.T) {
	h, root := unsealedHandler(t)

	// rate 1, burst 2 on secret/ — two requests pass, the third is throttled.
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/rate-limit/secretcap",
		`{"path":"secret/","rate":1,"burst":2}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("create quota: code=%d body=%s", rec.Code, rec.Body)
	}

	// The endpoint itself would 404 (no secret written), but the quota is enforced
	// in the middleware *before* the handler, so status reflects the quota.
	codes := []int{}
	for i := 0; i < 3; i++ {
		codes = append(codes, doAuth(t, h, "GET", "/v1/secret/data/none", "", root).Code)
	}
	if codes[0] == http.StatusTooManyRequests || codes[1] == http.StatusTooManyRequests {
		t.Fatalf("first two requests should pass the quota; codes=%v", codes)
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Errorf("third request should be 429 (burst spent); codes=%v", codes)
	}

	// A request on an unrelated path is not governed by the secret/ quota.
	if rec := doAuth(t, h, "GET", "/v1/sys/quotas/rate-limit/secretcap", "", root); rec.Code == http.StatusTooManyRequests {
		t.Error("unrelated path should not be throttled by the secret/ quota")
	}
}

type quotaConfigResp struct {
	Data struct {
		DefaultRate  float64 `json:"default_rate"`
		DefaultBurst float64 `json:"default_burst"`
	}
}

// TestQuota_ConfigCRUD sets and clears the global default via sys/quotas/config.
// A generous rate keeps the admin requests themselves from being throttled.
func TestQuota_ConfigCRUD(t *testing.T) {
	h, root := unsealedHandler(t)

	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/config", `{"default_rate":1000,"default_burst":2000}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("set config: code=%d body=%s", rec.Code, rec.Body)
	}
	cfg := decode[quotaConfigResp](t, doAuth(t, h, "GET", "/v1/sys/quotas/config", "", root))
	if cfg.Data.DefaultRate != 1000 || cfg.Data.DefaultBurst != 2000 {
		t.Errorf("config read = %+v, want rate 1000 burst 2000", cfg.Data)
	}

	// Clearing the default (rate 0) removes it.
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/config", `{"default_rate":0}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("clear config: code=%d", rec.Code)
	}
	cfg = decode[quotaConfigResp](t, doAuth(t, h, "GET", "/v1/sys/quotas/config", "", root))
	if cfg.Data.DefaultRate != 0 {
		t.Errorf("after clear, default_rate = %v, want 0", cfg.Data.DefaultRate)
	}
}

// TestQuota_DefaultEnforced verifies the config default governs a path that has
// no named quota (a strict default throttles within a short burst).
func TestQuota_DefaultEnforced(t *testing.T) {
	h, root := unsealedHandler(t)
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/config", `{"default_rate":1,"default_burst":2}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("set config: code=%d body=%s", rec.Code, rec.Body)
	}
	got429 := false
	for i := 0; i < 6; i++ {
		if do(t, h, "GET", "/v1/sys/seal-status", "").Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("default quota (burst 2) should throttle within 6 rapid requests")
	}
}

// TestQuota_LeaseCountCRUD exercises the lease-count quota endpoints.
func TestQuota_LeaseCountCRUD(t *testing.T) {
	h, root := unsealedHandler(t)
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/lease-count/dbcap", `{"path":"database/","max_leases":25}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body)
	}
	got := decode[struct {
		Data struct {
			Name, Type, Path string
			MaxLeases        int `json:"max_leases"`
		}
	}](t, doAuth(t, h, "GET", "/v1/sys/quotas/lease-count/dbcap", "", root))
	if got.Data.Type != "lease-count" || got.Data.Path != "database/" || got.Data.MaxLeases != 25 {
		t.Errorf("read fields: %+v", got.Data)
	}
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/lease-count/bad", `{"path":"database/","max_leases":0}`, root); rec.Code != http.StatusBadRequest {
		t.Errorf("zero max_leases: code=%d, want 400", rec.Code)
	}
	if rec := doAuth(t, h, "DELETE", "/v1/sys/quotas/lease-count/dbcap", "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d", rec.Code)
	}
}

// TestQuota_LeaseCountEnforced caps active DB leases and verifies the (N+1)th
// issuance is refused before a credential is created.
func TestQuota_LeaseCountEnforced(t *testing.T) {
	h, root, _ := unsealedDBHandler(t)
	configureAndRole(t, h, root)
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/lease-count/dbcap", `{"path":"database/","max_leases":2}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("create lease quota: code=%d body=%s", rec.Code, rec.Body)
	}
	c1 := doAuth(t, h, "GET", "/v1/database/creds/app", "", root).Code
	c2 := doAuth(t, h, "GET", "/v1/database/creds/app", "", root).Code
	c3 := doAuth(t, h, "GET", "/v1/database/creds/app", "", root)
	if c1 != http.StatusOK || c2 != http.StatusOK {
		t.Fatalf("first two creds should issue; got %d, %d", c1, c2)
	}
	if c3.Code != http.StatusTooManyRequests {
		t.Errorf("3rd cred should hit the lease-count cap; got %d body=%s", c3.Code, c3.Body)
	}
}

// TestQuota_ExceededMetric verifies a denial increments the exceeded counter,
// visible on the metrics endpoint.
func TestQuota_ExceededMetric(t *testing.T) {
	h, root := unsealedHandler(t)
	if rec := doAuth(t, h, "POST", "/v1/sys/quotas/rate-limit/secretcap",
		`{"path":"secret/","rate":1,"burst":1}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("create quota: code=%d body=%s", rec.Code, rec.Body)
	}
	// burst 1 → the second request is denied and counted.
	for i := 0; i < 3; i++ {
		doAuth(t, h, "GET", "/v1/secret/data/x", "", root)
	}
	body := do(t, h, "GET", "/v1/sys/metrics", "").Body.String()
	if !strings.Contains(body, `ubixvault_quota_exceeded_total{quota="secretcap"}`) {
		t.Errorf("metrics missing the quota-exceeded counter; body:\n%s", body)
	}
}
