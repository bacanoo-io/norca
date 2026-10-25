package api

import (
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/core"
)

type rootGenResponse struct {
	Started   bool   `json:"started"`
	Nonce     string `json:"nonce,omitempty"`
	Progress  int    `json:"progress"`
	Required  int    `json:"required"`
	Complete  bool   `json:"complete"`
	RootToken string `json:"root_token,omitempty"` // present only on completion
}

func toRootGenResponse(st *core.RootGenStatus) rootGenResponse {
	return rootGenResponse{
		Started:   st.Started,
		Nonce:     st.Nonce,
		Progress:  st.Progress,
		Required:  st.Required,
		Complete:  st.Complete,
		RootToken: st.RootToken,
	}
}

func (h *Handler) generateRootStatus(w http.ResponseWriter, r *http.Request) {
	st, err := h.core.GenerateRootStatus(r.Context())
	if err != nil {
		writeRootGenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRootGenResponse(st))
}

func (h *Handler) generateRootInit(w http.ResponseWriter, r *http.Request) {
	st, err := h.core.GenerateRootInit(r.Context())
	if err != nil {
		writeRootGenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRootGenResponse(st))
}

func (h *Handler) generateRootCancel(w http.ResponseWriter, _ *http.Request) {
	h.core.GenerateRootCancel()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) generateRootUpdate(w http.ResponseWriter, r *http.Request) {
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
	st, err := h.core.GenerateRootUpdate(r.Context(), req.Nonce, share)
	if err != nil {
		writeRootGenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRootGenResponse(st))
}

func writeRootGenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotInitialized):
		writeError(w, http.StatusBadRequest, "vault is not initialized")
	case errors.Is(err, core.ErrRootGenSealed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, core.ErrRootGenNotShamir),
		errors.Is(err, core.ErrRootGenNoRecovery),
		errors.Is(err, core.ErrRootGenNotStarted),
		errors.Is(err, core.ErrRootGenNonce),
		errors.Is(err, core.ErrInvalidShare),
		errors.Is(err, core.ErrUnsealFailed):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
