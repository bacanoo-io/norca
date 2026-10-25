package api

import (
	"context"
	"net/http"
	"time"
)

// registerMetrics wires the vault's operational gauges. Seal state is read from
// the core at scrape time so it is always current; uptime and build info are
// derived from the handler. Request counts are recorded in ServeHTTP.
func (h *Handler) registerMetrics() {
	h.metrics.RegisterGauge("ubixvault_build_info", "Build information; the value is always 1.",
		func() float64 { return 1 }, [2]string{"version", h.versionOrDev()})

	h.metrics.RegisterGauge("ubixvault_uptime_seconds", "Seconds since the server started.",
		func() float64 { return time.Since(h.startTime).Seconds() })

	h.metrics.RegisterGauge("ubixvault_initialized", "1 if the vault is initialized, else 0.",
		func() float64 { return boolGauge(h.sealState(func(i, _ bool) bool { return i })) })

	h.metrics.RegisterGauge("ubixvault_sealed", "1 if the vault is sealed, else 0.",
		func() float64 { return boolGauge(h.sealState(func(_, s bool) bool { return s })) })
}

// metricsEndpoint renders Prometheus text-format metrics. It is unauthenticated
// and exposes only operational series (seal state, uptime, request counts) — no
// secret material — so it is safe to scrape. Restrict it at the network layer as
// you would any /metrics endpoint.
func (h *Handler) metricsEndpoint(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.metrics.WriteProm(w)
}

// sealState reads the current seal status and projects it through sel. On error
// it reports false (metrics must never fail the scrape).
func (h *Handler) sealState(sel func(initialized, sealed bool) bool) bool {
	st, err := h.core.Status(context.Background())
	if err != nil {
		return false
	}
	return sel(st.Initialized, st.Sealed)
}

func (h *Handler) versionOrDev() string {
	if h.version == "" {
		return "unknown"
	}
	return h.version
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
