package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/kubeauth"
)

type k8sConfigRequest struct {
	Host        string `json:"kubernetes_host"`
	CACert      string `json:"kubernetes_ca_cert"`
	ReviewerJWT string `json:"token_reviewer_jwt"`
}

type k8sRoleRequest struct {
	BoundServiceAccountNames      []string      `json:"bound_service_account_names"`
	BoundServiceAccountNamespaces []string      `json:"bound_service_account_namespaces"`
	Policies                      []string      `json:"policies"`
	TTL                           vaultDuration `json:"ttl"`
	TokenMaxTTL                   vaultDuration `json:"token_max_ttl"`
}

type k8sLoginRequest struct {
	Role string `json:"role"`
	JWT  string `json:"jwt"`
}

func (h *Handler) k8sConfigure(w http.ResponseWriter, r *http.Request) {
	var req k8sConfigRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Host == "" {
		writeError(w, http.StatusBadRequest, "kubernetes_host is required")
		return
	}
	cfg := kubeauth.Config{Host: req.Host, CACert: req.CACert, ReviewerJWT: req.ReviewerJWT}
	if err := h.kubernetes.Configure(r.Context(), cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) k8sWriteRole(w http.ResponseWriter, r *http.Request) {
	var req k8sRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var ttl time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(string(req.TTL))
		if err != nil {
			writeError(w, http.StatusBadRequest, "ttl must be a duration (e.g. \"1h\")")
			return
		}
		ttl = d
	}
	maxTTL, ok := parseMaxTTL(w, req.TokenMaxTTL, ttl)
	if !ok {
		return
	}
	role := kubeauth.Role{
		BoundServiceAccountNames:      req.BoundServiceAccountNames,
		BoundServiceAccountNamespaces: req.BoundServiceAccountNamespaces,
		Policies:                      req.Policies,
		TTL:                           ttl,
		TokenMaxTTL:                   maxTTL,
	}
	if err := h.kubernetes.WriteRole(r.Context(), r.PathValue("name"), role); err != nil {
		writeKubeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) k8sReadRole(w http.ResponseWriter, r *http.Request) {
	role, err := h.kubernetes.ReadRole(r.Context(), r.PathValue("name"))
	if err != nil {
		writeKubeError(w, err)
		return
	}
	writeData(w, map[string]any{
		"bound_service_account_names":      role.BoundServiceAccountNames,
		"bound_service_account_namespaces": role.BoundServiceAccountNamespaces,
		"policies":                         role.Policies,
		"ttl":                              role.TTL.String(),
		"token_max_ttl":                    role.TokenMaxTTL.String(),
	})
}

func (h *Handler) k8sListRoles(w http.ResponseWriter, r *http.Request) {
	names, err := h.kubernetes.ListRoles(r.Context())
	if err != nil {
		writeKubeError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": names})
}

func (h *Handler) k8sDeleteRole(w http.ResponseWriter, r *http.Request) {
	if err := h.kubernetes.DeleteRole(r.Context(), r.PathValue("name")); err != nil {
		writeKubeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// k8sLogin is unauthenticated: the ServiceAccount JWT is the credential.
func (h *Handler) k8sLogin(w http.ResponseWriter, r *http.Request) {
	var req k8sLoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role == "" || req.JWT == "" {
		writeError(w, http.StatusBadRequest, "role and jwt are required")
		return
	}
	tok, err := h.kubernetes.Login(r.Context(), req.Role, req.JWT)
	if err != nil {
		writeKubeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth": map[string]any{
			"client_token": tok.ID,
			"policies":     tok.Policies,
		},
	})
}

func writeKubeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, kubeauth.ErrDenied):
		writeError(w, http.StatusForbidden, "permission denied")
	case errors.Is(err, kubeauth.ErrRoleNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, kubeauth.ErrNotConfigured), errors.Is(err, kubeauth.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
