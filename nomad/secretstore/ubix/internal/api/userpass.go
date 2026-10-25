package api

import (
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/userpass"
)

type userpassUserRequest struct {
	Password    string        `json:"password"`
	Policies    []string      `json:"policies"`
	TokenTTL    vaultDuration `json:"token_ttl"`
	TokenMaxTTL vaultDuration `json:"token_max_ttl"`
}

type userpassLoginRequest struct {
	Password string `json:"password"`
}

func (h *Handler) userpassWriteUser(w http.ResponseWriter, r *http.Request) {
	var req userpassUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ttl, ok := parseOptionalDuration(w, string(req.TokenTTL), "token_ttl")
	if !ok {
		return
	}
	maxTTL, ok := parseMaxTTL(w, req.TokenMaxTTL, ttl)
	if !ok {
		return
	}
	if err := h.userpass.WriteUser(r.Context(), r.PathValue("username"), req.Password, req.Policies, ttl, maxTTL); err != nil {
		writeUserpassError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) userpassReadUser(w http.ResponseWriter, r *http.Request) {
	info, err := h.userpass.ReadUser(r.Context(), r.PathValue("username"))
	if err != nil {
		writeUserpassError(w, err)
		return
	}
	writeData(w, map[string]any{"policies": info.Policies, "token_ttl": info.TokenTTL.String(), "token_max_ttl": info.TokenMaxTTL.String()})
}

func (h *Handler) userpassListUsers(w http.ResponseWriter, r *http.Request) {
	names, err := h.userpass.ListUsers(r.Context())
	if err != nil {
		writeUserpassError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": names})
}

func (h *Handler) userpassDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := h.userpass.DeleteUser(r.Context(), r.PathValue("username")); err != nil {
		writeUserpassError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// userpassLogin is unauthenticated: the password is the credential.
func (h *Handler) userpassLogin(w http.ResponseWriter, r *http.Request) {
	var req userpassLoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tok, err := h.userpass.Login(r.Context(), r.PathValue("username"), req.Password)
	if err != nil {
		writeUserpassError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenAuthResponse(tok))
}

func writeUserpassError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, userpass.ErrDenied):
		writeError(w, http.StatusForbidden, "permission denied")
	case errors.Is(err, userpass.ErrUserNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, userpass.ErrInvalidName), errors.Is(err, userpass.ErrInvalidConfig):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
