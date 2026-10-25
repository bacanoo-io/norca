package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/wrapping"
)

// wrapTTLHeader carries the requested wrap TTL, mirroring HashiCorp Vault.
const wrapTTLHeader = "X-Vault-Wrap-TTL"

// sysWrappingWrap wraps the JSON request body in a single-use token. The TTL
// comes from the X-Vault-Wrap-TTL header (a Go duration like "5m" or a bare
// number of seconds); absent or non-positive means the default TTL.
func (h *Handler) sysWrappingWrap(w http.ResponseWriter, r *http.Request) {
	ttl, ok := parseWrapTTL(w, r.Header.Get(wrapTTLHeader))
	if !ok {
		return
	}
	var payload json.RawMessage
	if !decodeJSON(w, r, &payload) {
		return
	}
	info, err := h.wrapping.Wrap(r.Context(), payload, ttl)
	if err != nil {
		writeWrappingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"wrap_info": map[string]any{
			"token":         info.Token,
			"ttl":           int(info.TTL.Seconds()),
			"creation_time": info.CreationTime.Format(time.RFC3339),
			"expires_at":    info.ExpiresAt.Format(time.RFC3339),
		},
	})
}

// sysWrappingUnwrap consumes a wrapping token and returns its payload once.
func (h *Handler) sysWrappingUnwrap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	payload, err := h.wrapping.Unwrap(r.Context(), req.Token)
	if err != nil {
		writeWrappingError(w, err)
		return
	}
	writeData(w, payload)
}

// parseWrapTTL accepts a Go duration ("5m") or a bare integer number of seconds.
func parseWrapTTL(w http.ResponseWriter, s string) (time.Duration, bool) {
	if s == "" {
		return 0, true
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			writeError(w, http.StatusBadRequest, wrapTTLHeader+" must not be negative")
			return 0, false
		}
		return d, true
	}
	if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	writeError(w, http.StatusBadRequest, wrapTTLHeader+` must be a duration ("5m") or seconds`)
	return 0, false
}

func writeWrappingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, wrapping.ErrNotFound), errors.Is(err, wrapping.ErrExpired):
		// Don't distinguish absent from expired to a caller holding a bad token.
		writeError(w, http.StatusBadRequest, "wrapping token is invalid or already used")
	case errors.Is(err, wrapping.ErrInvalidTTL):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
