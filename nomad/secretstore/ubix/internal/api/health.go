package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

type healthResponse struct {
	Initialized   bool   `json:"initialized"`
	Sealed        bool   `json:"sealed"`
	Standby       bool   `json:"standby"`
	Version       string `json:"version,omitempty"`
	ServerTimeUTC int64  `json:"server_time_utc"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// health reports readiness. The HTTP status encodes it so load balancers and
// probes can act without parsing the body:
//   - 200 initialized, unsealed and active (ready)
//   - 429 an unsealed HA standby
//   - 503 sealed (not ready)
//   - 501 not initialized
//
// As in Vault, query parameters override each code — activecode, standbycode,
// sealedcode, uninitcode — and standbyok=true reports a standby with the
// active code, which is what a readiness probe wants when standbys can take
// traffic. perfstandbyok is accepted and ignored (no performance standbys).
//
// For process liveness (which must ignore seal state) use livez instead. This
// endpoint is unauthenticated and excluded from audit logging (see ServeHTTP).
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	codes := map[string]int{
		"activecode":  http.StatusOK,
		"standbycode": http.StatusTooManyRequests,
		"sealedcode":  http.StatusServiceUnavailable,
		"uninitcode":  http.StatusNotImplemented,
	}
	for name := range codes {
		v := q.Get(name)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 || n > 599 {
			writeError(w, http.StatusBadRequest, "invalid "+name+": "+v)
			return
		}
		codes[name] = n
	}
	standbyOK, _ := strconv.ParseBool(q.Get("standbyok"))

	st, err := h.core.Status(r.Context())
	if err != nil {
		// Storage temporarily unreachable → not ready (503), not a 500. A
		// 503 drops readiness so traffic stops, and recovers on its own when
		// storage returns — the process must not die over a storage blip.
		if errors.Is(err, storage.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "storage backend unavailable")
			return
		}
		writeInternal(w, err)
		return
	}

	standby := !st.Sealed && h.core.Standby()
	code := codes["activecode"]
	switch {
	case !st.Initialized:
		code = codes["uninitcode"]
	case st.Sealed:
		code = codes["sealedcode"]
	case standby && !standbyOK:
		code = codes["standbycode"]
	}

	now := time.Now().UTC()
	writeJSON(w, code, healthResponse{
		Initialized:   st.Initialized,
		Sealed:        st.Sealed,
		Standby:       standby,
		Version:       h.version,
		ServerTimeUTC: now.Unix(),
		UptimeSeconds: int64(now.Sub(h.startTime).Seconds()),
	})
}

// livez reports process liveness. Unlike health (whose status encodes
// readiness — 501 uninitialized, 503 sealed), livez returns 200 whenever the
// HTTP server is serving, regardless of init/seal state. That makes it safe as
// a Kubernetes liveness probe: it never fails during the normal sealed window,
// so it will not crash-loop a sealed vault, while still confirming the HTTP
// server actually responds (which a bare TCP probe cannot). Unauthenticated and
// audit-exempt, like health.
func (h *Handler) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"alive": true})
}
