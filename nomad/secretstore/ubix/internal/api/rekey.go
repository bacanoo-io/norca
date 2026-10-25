package api

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/core"
)

type rekeyResponse struct {
	Started      bool     `json:"started"`
	Nonce        string   `json:"nonce,omitempty"`
	Progress     int      `json:"progress"`
	Required     int      `json:"required"`
	NewShares    int      `json:"new_shares"`
	NewThreshold int      `json:"new_threshold"`
	Complete     bool     `json:"complete"`
	Keys         []string `json:"keys,omitempty"`        // new unseal shares (hex), only on completion
	KeysBase64   []string `json:"keys_base64,omitempty"` // same, base64
}

func toRekeyResponse(st *core.RekeyStatus) rekeyResponse {
	resp := rekeyResponse{
		Started:      st.Started,
		Nonce:        st.Nonce,
		Progress:     st.Progress,
		Required:     st.Required,
		NewShares:    st.NewShares,
		NewThreshold: st.NewThreshold,
		Complete:     st.Complete,
	}
	for _, k := range st.Keys {
		resp.Keys = append(resp.Keys, hex.EncodeToString(k))
		resp.KeysBase64 = append(resp.KeysBase64, base64.StdEncoding.EncodeToString(k))
	}
	return resp
}

func (h *Handler) rekeyStatus(w http.ResponseWriter, r *http.Request) {
	st, err := h.core.RekeyStatus(r.Context())
	if err != nil {
		writeRekeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRekeyResponse(st))
}

func (h *Handler) rekeyInit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SecretShares    int `json:"secret_shares"`
		SecretThreshold int `json:"secret_threshold"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	st, err := h.core.RekeyInit(r.Context(), req.SecretShares, req.SecretThreshold)
	if err != nil {
		writeRekeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRekeyResponse(st))
}

func (h *Handler) rekeyCancel(w http.ResponseWriter, _ *http.Request) {
	h.core.RekeyCancel()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) rekeyUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nonce string `json:"nonce"`
		Key   string `json:"key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	share, err := decodeShare(req.Key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "key must be valid hex or base64")
		return
	}
	st, err := h.core.RekeyUpdate(r.Context(), req.Nonce, share)
	if err != nil {
		writeRekeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRekeyResponse(st))
}

func writeRekeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotInitialized):
		writeError(w, http.StatusBadRequest, "vault is not initialized")
	case errors.Is(err, core.ErrRekeySealed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, core.ErrRekeyNotShamir),
		errors.Is(err, core.ErrRekeyNotStarted),
		errors.Is(err, core.ErrRekeyNonce),
		errors.Is(err, core.ErrInvalidConfig),
		errors.Is(err, core.ErrInvalidShare),
		errors.Is(err, core.ErrUnsealFailed):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
