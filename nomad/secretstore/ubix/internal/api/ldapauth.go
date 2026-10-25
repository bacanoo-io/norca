package api

import (
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/ldapauth"
)

type ldapConfigRequest struct {
	URL          string        `json:"url"`
	StartTLS     bool          `json:"starttls"`
	InsecureTLS  bool          `json:"insecure_tls"`
	Certificate  string        `json:"certificate"` // PEM CA bundle (Vault's field name)
	BindDN       string        `json:"bind_dn"`
	BindPassword string        `json:"bind_password"`
	UserDN       string        `json:"user_dn"`
	UserAttr     string        `json:"user_attr"`
	GroupDN      string        `json:"group_dn"`
	GroupAttr    string        `json:"group_attr"`
	GroupFilter  string        `json:"group_filter"`
	TokenTTL     vaultDuration `json:"token_ttl"`
	TokenMaxTTL  vaultDuration `json:"token_max_ttl"`
}

type ldapGroupRequest struct {
	Policies []string `json:"policies"`
}

type ldapLoginRequest struct {
	Password string `json:"password"`
}

func (h *Handler) ldapConfigure(w http.ResponseWriter, r *http.Request) {
	var req ldapConfigRequest
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
	err := h.ldap.Configure(r.Context(), ldapauth.Config{
		URL:          req.URL,
		StartTLS:     req.StartTLS,
		InsecureTLS:  req.InsecureTLS,
		Certificate:  req.Certificate,
		BindDN:       req.BindDN,
		BindPassword: req.BindPassword,
		UserDN:       req.UserDN,
		UserAttr:     req.UserAttr,
		GroupDN:      req.GroupDN,
		GroupAttr:    req.GroupAttr,
		GroupFilter:  req.GroupFilter,
		TokenTTL:     ttl,
		TokenMaxTTL:  maxTTL,
	})
	if err != nil {
		writeLDAPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ldapReadConfig returns the config with the bind password redacted.
func (h *Handler) ldapReadConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.ldap.ReadConfig(r.Context())
	if err != nil {
		writeLDAPError(w, err)
		return
	}
	bindPasswordSet := cfg.BindPassword != ""
	writeData(w, map[string]any{
		"url":               cfg.URL,
		"starttls":          cfg.StartTLS,
		"insecure_tls":      cfg.InsecureTLS,
		"certificate":       cfg.Certificate, // public CA material, returned as Vault does
		"bind_dn":           cfg.BindDN,
		"bind_password_set": bindPasswordSet, // never echo the secret
		"user_dn":           cfg.UserDN,
		"user_attr":         cfg.UserAttr,
		"group_dn":          cfg.GroupDN,
		"group_attr":        cfg.GroupAttr,
		"group_filter":      cfg.GroupFilter,
		"token_ttl":         cfg.TokenTTL.String(),
		"token_max_ttl":     cfg.TokenMaxTTL.String(),
	})
}

func (h *Handler) ldapWriteGroup(w http.ResponseWriter, r *http.Request) {
	var req ldapGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.ldap.WriteGroup(r.Context(), r.PathValue("name"), req.Policies); err != nil {
		writeLDAPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ldapReadGroup(w http.ResponseWriter, r *http.Request) {
	policies, err := h.ldap.ReadGroup(r.Context(), r.PathValue("name"))
	if err != nil {
		writeLDAPError(w, err)
		return
	}
	writeData(w, map[string]any{"policies": policies})
}

func (h *Handler) ldapListGroups(w http.ResponseWriter, r *http.Request) {
	names, err := h.ldap.ListGroups(r.Context())
	if err != nil {
		writeLDAPError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": names})
}

func (h *Handler) ldapDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := h.ldap.DeleteGroup(r.Context(), r.PathValue("name")); err != nil {
		writeLDAPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ldapLogin is unauthenticated: the directory password is the credential.
func (h *Handler) ldapLogin(w http.ResponseWriter, r *http.Request) {
	var req ldapLoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tok, err := h.ldap.Login(r.Context(), r.PathValue("username"), req.Password)
	if err != nil {
		writeLDAPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenAuthResponse(tok))
}

func writeLDAPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, ldapauth.ErrDenied):
		writeError(w, http.StatusForbidden, "permission denied")
	case errors.Is(err, ldapauth.ErrNotConfigured):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ldapauth.ErrInvalidName), errors.Is(err, ldapauth.ErrInvalidConfig),
		errors.Is(err, ldapauth.ErrInvalidCertificate), errors.Is(err, ldapauth.ErrCertificateWithInsecureTLS):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
