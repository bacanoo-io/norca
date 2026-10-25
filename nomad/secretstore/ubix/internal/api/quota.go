package api

import (
	"errors"
	"net/http"
	"sort"

	"github.com/cwolsen7905/ubixvault/internal/quota"
)

// quotaWrite creates or replaces a rate-limit quota. Body: {"path","rate","burst"};
// the name comes from the path. Root/ACL-gated like other sys endpoints.
func (h *Handler) quotaWrite(w http.ResponseWriter, r *http.Request) {
	_ = h.quotas.EnsureLoaded(r.Context())
	var req struct {
		Path  string  `json:"path"`
		Rate  float64 `json:"rate"`
		Burst float64 `json:"burst"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	q := quota.RateLimitQuota{Name: r.PathValue("name"), Path: req.Path, Rate: req.Rate, Burst: req.Burst}
	if err := h.quotas.Set(r.Context(), q); err != nil {
		writeQuotaError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// quotaRead returns a single rate-limit quota.
func (h *Handler) quotaRead(w http.ResponseWriter, r *http.Request) {
	_ = h.quotas.EnsureLoaded(r.Context())
	q, err := h.quotas.Get(r.PathValue("name"))
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	burst := q.Burst
	if burst <= 0 {
		burst = q.Rate
	}
	writeData(w, map[string]any{
		"name":  q.Name,
		"type":  "rate-limit",
		"path":  q.Path,
		"rate":  q.Rate,
		"burst": burst,
	})
}

// quotaDelete removes a rate-limit quota (absent is not an error).
func (h *Handler) quotaDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.quotas.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeQuotaError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// quotaList returns the names of all rate-limit quotas.
func (h *Handler) quotaList(w http.ResponseWriter, r *http.Request) {
	_ = h.quotas.EnsureLoaded(r.Context())
	specs := h.quotas.List()
	names := make([]string, 0, len(specs))
	for _, q := range specs {
		names = append(names, q.Name)
	}
	sort.Strings(names)
	writeData(w, map[string]any{"keys": names})
}

// quotaConfigRead returns the global quota config (the default quota's rate/burst).
func (h *Handler) quotaConfigRead(w http.ResponseWriter, r *http.Request) {
	_ = h.quotas.EnsureLoaded(r.Context())
	rate, burst := h.quotas.DefaultConfig()
	writeData(w, map[string]any{"default_rate": rate, "default_burst": burst})
}

// quotaConfigWrite sets the default (global) quota. A non-positive default_rate
// clears it. burst defaults to rate when omitted.
func (h *Handler) quotaConfigWrite(w http.ResponseWriter, r *http.Request) {
	_ = h.quotas.EnsureLoaded(r.Context())
	var req struct {
		DefaultRate  float64 `json:"default_rate"`
		DefaultBurst float64 `json:"default_burst"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.quotas.SetConfig(r.Context(), req.DefaultRate, req.DefaultBurst); err != nil {
		writeQuotaError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// leaseQuotaWrite creates or replaces a lease-count quota. Body: {"path","max_leases"}.
func (h *Handler) leaseQuotaWrite(w http.ResponseWriter, r *http.Request) {
	_ = h.quotas.EnsureLoaded(r.Context())
	var req struct {
		Path      string `json:"path"`
		MaxLeases int    `json:"max_leases"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	q := quota.LeaseCountQuota{Name: r.PathValue("name"), Path: req.Path, MaxLeases: req.MaxLeases}
	if err := h.quotas.SetLeaseCount(r.Context(), q); err != nil {
		writeQuotaError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// leaseQuotaRead returns a single lease-count quota.
func (h *Handler) leaseQuotaRead(w http.ResponseWriter, r *http.Request) {
	_ = h.quotas.EnsureLoaded(r.Context())
	q, err := h.quotas.GetLeaseCount(r.PathValue("name"))
	if err != nil {
		writeQuotaError(w, err)
		return
	}
	writeData(w, map[string]any{
		"name":       q.Name,
		"type":       "lease-count",
		"path":       q.Path,
		"max_leases": q.MaxLeases,
	})
}

// leaseQuotaDelete removes a lease-count quota (absent is not an error).
func (h *Handler) leaseQuotaDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.quotas.DeleteLeaseCount(r.Context(), r.PathValue("name")); err != nil {
		writeQuotaError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// leaseQuotaList returns the names of all lease-count quotas.
func (h *Handler) leaseQuotaList(w http.ResponseWriter, r *http.Request) {
	_ = h.quotas.EnsureLoaded(r.Context())
	specs := h.quotas.ListLeaseCount()
	names := make([]string, 0, len(specs))
	for _, q := range specs {
		names = append(names, q.Name)
	}
	sort.Strings(names)
	writeData(w, map[string]any{"keys": names})
}

func writeQuotaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, quota.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, quota.ErrInvalidName), errors.Is(err, quota.ErrInvalidRate),
		errors.Is(err, quota.ErrInvalidPath), errors.Is(err, quota.ErrInvalidMax):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
