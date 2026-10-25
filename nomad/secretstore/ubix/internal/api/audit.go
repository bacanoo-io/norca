package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/cwolsen7905/ubixvault/internal/audit"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// auditHMACKeyPath is where the audit token-HMAC key lives in the barrier. One
// key per vault, not per process, so a token HMACs the same on every replica and
// across restarts.
const auditHMACKeyPath = "sys/audit/hmac-key"

// ServeHTTP dispatches to the configured routes. When audit logging is enabled it
// records a request entry before handling and a response entry after. Request
// auditing is fail-closed: if the entry cannot be recorded, the request is
// refused (500) and never processed, so nothing proceeds unaudited.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Vault accepts a list as either the LIST method or GET with list=true, and
	// its own client sends the latter. Normalize first, so routing, the ACL
	// check (list capability), rate limits, audit and HA forwarding all see LIST.
	if r.Method == http.MethodGet && r.URL.Query().Get("list") == "true" {
		r.Method = "LIST"
	}

	// Wrap once so both metrics and audit see the final status code; count every
	// request toward metrics regardless of the decisions below.
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() { h.metrics.ObserveRequest(rec.status) }()

	// An HA standby answers only lifecycle and health endpoints itself; anything
	// else belongs to the active replica, which audits and rate-limits it there.
	if h.core.Standby() && !standbyEndpoint(r.URL.Path) {
		if h.forward != nil {
			h.forward.ServeHTTP(rec, r)
			return
		}
		writeError(rec, http.StatusServiceUnavailable, "this replica is a standby; send requests to the active replica")
		return
	}

	// Rate limiting: throttle authenticated/lifecycle endpoints (health, metrics,
	// and the console are exempt so probes/scrapers/browsers aren't blocked).
	// Rate-limit quotas: the default (global) quota applies to every non-public
	// path — even while sealed, so init/unseal can't be brute-forced — while
	// named, path-scoped quotas live in the barrier and take effect once the
	// vault is unsealed (EnsureLoaded is a cheap no-op after its first success).
	if !publicEndpoint(r.URL.Path) {
		if !h.core.Barrier().Sealed() {
			_ = h.quotas.EnsureLoaded(r.Context())
		}
		if ok, name := h.quotas.Allow(apiPath(r), h.clientKey(r)); !ok {
			rec.Header().Set("Retry-After", "1")
			if name == "" {
				writeError(rec, http.StatusTooManyRequests, "rate limit exceeded")
			} else {
				writeError(rec, http.StatusTooManyRequests, "rate-limit quota exceeded: "+name)
			}
			return
		}
	}

	// Public endpoints (health, metrics, console) are not audited.
	if h.audit == nil || publicEndpoint(r.URL.Path) {
		h.mux.ServeHTTP(rec, r)
		return
	}

	base := audit.Entry{
		Operation:   operationForMethod(r.Method),
		Path:        apiPath(r),
		ClientToken: r.Header.Get(tokenHeader),
		RemoteAddr:  r.RemoteAddr,
	}

	// Once unsealed, entries must carry the vault's token HMAC; if it cannot be
	// loaded the request is refused, like any other audit failure.
	if !h.core.Barrier().Sealed() {
		if err := h.ensureAuditKey(r.Context()); err != nil {
			writeError(rec, http.StatusInternalServerError, "audit logging failed")
			return
		}
	}

	req := base
	if err := h.audit.LogRequest(r.Context(), &req); err != nil {
		writeError(rec, http.StatusInternalServerError, "audit logging failed")
		return
	}

	h.mux.ServeHTTP(rec, r)

	resp := base
	resp.StatusCode = rec.status
	// The request has already been served; a response-audit failure cannot unwind
	// it, so it is best-effort (the fail-closed guarantee is on the request).
	_ = h.audit.LogResponse(r.Context(), &resp)
}

// ensureAuditKey installs the barrier's audit HMAC key in the audit broker,
// creating the key on first use. It is a no-op once installed, until a seal
// clears it (resetBarrierCaches).
func (h *Handler) ensureAuditKey(ctx context.Context) error {
	h.auditKeyMu.Lock()
	defer h.auditKeyMu.Unlock()
	if h.auditKeySet {
		return nil
	}
	b := h.core.Barrier()
	entry, err := b.Get(ctx, auditHMACKeyPath)
	if err != nil {
		return fmt.Errorf("audit: read hmac key: %w", err)
	}
	var key []byte
	if entry != nil {
		key = entry.Value
	} else {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return fmt.Errorf("audit: generate hmac key: %w", err)
		}
		if err := b.Put(ctx, &storage.Entry{Key: auditHMACKeyPath, Value: key}); err != nil {
			return fmt.Errorf("audit: persist hmac key: %w", err)
		}
	}
	h.audit.SetHMACKey(key)
	h.auditKeySet = true
	return nil
}

// resetBarrierCaches drops state cached from the barrier: loaded quotas, the
// database plugin's connection, the Kubernetes TokenReview client, cached JWKS,
// and the audit HMAC key. Registered with core.OnSeal; the HA step-down path
// will call it too (docs/design/ha-active-standby.md). Each is reloaded lazily
// on first use after the next unseal.
func (h *Handler) resetBarrierCaches() {
	h.quotas.Reset()
	h.database.Reset()
	h.kubernetes.Reset()
	h.jwtauth.Reset()
	if h.audit != nil {
		h.auditKeyMu.Lock()
		h.audit.SetHMACKey(nil)
		h.auditKeySet = false
		h.auditKeyMu.Unlock()
	}
}

// publicEndpoint reports whether a path is an unauthenticated, non-sensitive
// endpoint — the health/metrics endpoints and the static console assets. These
// are exempt from both auditing and rate limiting. (The /v1/* calls the console
// makes on the operator's behalf are audited and rate-limited normally.)
func publicEndpoint(path string) bool {
	if strings.HasPrefix(path, "/ui/") {
		return true
	}
	switch path {
	case "/", "/ui", "/v1/sys/health", "/v1/sys/livez", "/v1/sys/metrics":
		return true
	default:
		return false
	}
}

// standbyEndpoint reports whether a standby replica serves path itself: the
// public endpoints, and the lifecycle calls that are per replica (unsealing and
// sealing this process, reading its seal status) or that take the HA lock
// themselves (init).
func standbyEndpoint(path string) bool {
	if publicEndpoint(path) {
		return true
	}
	switch path {
	case "/v1/sys/seal-status", "/v1/sys/unseal", "/v1/sys/seal", "/v1/sys/init", "/v1/sys/leader":
		return true
	default:
		return false
	}
}

// clientKey identifies the caller for rate limiting: the direct peer IP, or the
// leftmost X-Forwarded-For entry when trustForwarded is set (behind a proxy).
func (h *Handler) clientKey(r *http.Request) string {
	if h.trustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// operationForMethod maps an HTTP method to an audit operation name.
func operationForMethod(method string) string {
	switch method {
	case http.MethodGet:
		return "read"
	case "LIST":
		return "list"
	case http.MethodDelete:
		return "delete"
	case http.MethodPost, http.MethodPut:
		return "update"
	default:
		return strings.ToLower(method)
	}
}

// statusRecorder captures the response status code for auditing.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}
