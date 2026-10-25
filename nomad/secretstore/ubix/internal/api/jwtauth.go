package api

import (
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/jwtauth"
)

type jwtConfigRequest struct {
	JWKSURL           string   `json:"jwks_url"`
	OIDCDiscoveryURL  string   `json:"oidc_discovery_url"`
	ValidationPubKeys []string `json:"jwt_validation_pubkeys"`
	BoundIssuer       string   `json:"bound_issuer"`
	GroupsClaim       string   `json:"groups_claim"`
}

type jwtRoleRequest struct {
	BoundAudiences []string          `json:"bound_audiences"`
	BoundClaims    map[string]string `json:"bound_claims"`
	Policies       []string          `json:"policies"`
	TokenTTL       vaultDuration     `json:"token_ttl"`
	TokenMaxTTL    vaultDuration     `json:"token_max_ttl"`
}

type jwtLoginRequest struct {
	Role string `json:"role"`
	JWT  string `json:"jwt"`
}

func (h *Handler) jwtConfigure(w http.ResponseWriter, r *http.Request) {
	var req jwtConfigRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	err := h.jwtauth.Configure(r.Context(), jwtauth.Config{
		JWKSURL:           req.JWKSURL,
		OIDCDiscoveryURL:  req.OIDCDiscoveryURL,
		ValidationPubKeys: req.ValidationPubKeys,
		BoundIssuer:       req.BoundIssuer,
		GroupsClaim:       req.GroupsClaim,
	})
	if err != nil {
		writeJWTError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) jwtWriteRole(w http.ResponseWriter, r *http.Request) {
	var req jwtRoleRequest
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
	err := h.jwtauth.WriteRole(r.Context(), r.PathValue("name"), jwtauth.Role{
		BoundAudiences: req.BoundAudiences,
		BoundClaims:    req.BoundClaims,
		Policies:       req.Policies,
		TokenTTL:       ttl,
		TokenMaxTTL:    maxTTL,
	})
	if err != nil {
		writeJWTError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) jwtReadRole(w http.ResponseWriter, r *http.Request) {
	role, err := h.jwtauth.ReadRole(r.Context(), r.PathValue("name"))
	if err != nil {
		writeJWTError(w, err)
		return
	}
	writeData(w, map[string]any{
		"bound_audiences": role.BoundAudiences,
		"bound_claims":    role.BoundClaims,
		"policies":        role.Policies,
		"token_ttl":       role.TokenTTL.String(),
		"token_max_ttl":   role.TokenMaxTTL.String(),
	})
}

func (h *Handler) jwtListRoles(w http.ResponseWriter, r *http.Request) {
	names, err := h.jwtauth.ListRoles(r.Context())
	if err != nil {
		writeJWTError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": names})
}

func (h *Handler) jwtDeleteRole(w http.ResponseWriter, r *http.Request) {
	if err := h.jwtauth.DeleteRole(r.Context(), r.PathValue("name")); err != nil {
		writeJWTError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// jwtLogin is unauthenticated: the signed JWT is the credential.
func (h *Handler) jwtLogin(w http.ResponseWriter, r *http.Request) {
	var req jwtLoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tok, err := h.jwtauth.Login(r.Context(), req.Role, req.JWT)
	if err != nil {
		writeJWTError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenAuthResponse(tok))
}

func writeJWTError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, jwtauth.ErrDenied):
		writeError(w, http.StatusForbidden, "permission denied")
	case errors.Is(err, jwtauth.ErrNotConfigured):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, jwtauth.ErrRoleNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, jwtauth.ErrInvalidName), errors.Is(err, jwtauth.ErrInvalidConfig):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
