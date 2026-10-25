package api

import (
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/pki"
)

type pkiRootRequest struct {
	CommonName string        `json:"common_name"`
	TTL        vaultDuration `json:"ttl"`
	KeyType    string        `json:"key_type"`
	KeyBits    int           `json:"key_bits"`
}

type pkiRoleRequest struct {
	AllowedDomains  []string      `json:"allowed_domains"`
	AllowSubdomains bool          `json:"allow_subdomains"`
	MaxTTL          vaultDuration `json:"max_ttl"`
	KeyType         string        `json:"key_type"`
	KeyBits         int           `json:"key_bits"`
}

type pkiIssueRequest struct {
	CommonName string        `json:"common_name"`
	AltNames   []string      `json:"alt_names"`
	TTL        vaultDuration `json:"ttl"`
}

func (h *Handler) pkiGenerateRoot(w http.ResponseWriter, r *http.Request) {
	var req pkiRootRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CommonName == "" {
		writeError(w, http.StatusBadRequest, "common_name is required")
		return
	}
	ttl, ok := parseOptionalDuration(w, string(req.TTL), "ttl")
	if !ok {
		return
	}
	certPEM, err := h.pki.GenerateRoot(r.Context(), pki.RootConfig{
		CommonName: req.CommonName, TTL: ttl, KeyType: req.KeyType, KeyBits: req.KeyBits,
	})
	if err != nil {
		writePKIError(w, err)
		return
	}
	writeData(w, map[string]any{"certificate": certPEM})
}

func (h *Handler) pkiReadCA(w http.ResponseWriter, r *http.Request) {
	certPEM, err := h.pki.CACertPEM(r.Context())
	if err != nil {
		writePKIError(w, err)
		return
	}
	writeData(w, map[string]any{"certificate": certPEM})
}

func (h *Handler) pkiWriteRole(w http.ResponseWriter, r *http.Request) {
	var req pkiRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	maxTTL, ok := parseOptionalDuration(w, string(req.MaxTTL), "max_ttl")
	if !ok {
		return
	}
	role := pki.Role{
		AllowedDomains:  req.AllowedDomains,
		AllowSubdomains: req.AllowSubdomains,
		MaxTTL:          maxTTL,
		KeyType:         req.KeyType,
		KeyBits:         req.KeyBits,
	}
	if err := h.pki.WriteRole(r.Context(), r.PathValue("name"), role); err != nil {
		writePKIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) pkiReadRole(w http.ResponseWriter, r *http.Request) {
	role, err := h.pki.ReadRole(r.Context(), r.PathValue("name"))
	if err != nil {
		writePKIError(w, err)
		return
	}
	writeData(w, map[string]any{
		"allowed_domains":  role.AllowedDomains,
		"allow_subdomains": role.AllowSubdomains,
		"max_ttl":          role.MaxTTL.String(),
		"key_type":         role.KeyType,
		"key_bits":         role.KeyBits,
	})
}

func (h *Handler) pkiListRoles(w http.ResponseWriter, r *http.Request) {
	names, err := h.pki.ListRoles(r.Context())
	if err != nil {
		writePKIError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": names})
}

func (h *Handler) pkiDeleteRole(w http.ResponseWriter, r *http.Request) {
	if err := h.pki.DeleteRole(r.Context(), r.PathValue("name")); err != nil {
		writePKIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) pkiIssue(w http.ResponseWriter, r *http.Request) {
	var req pkiIssueRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ttl, ok := parseOptionalDuration(w, string(req.TTL), "ttl")
	if !ok {
		return
	}
	issued, err := h.pki.Issue(r.Context(), r.PathValue("role"), pki.IssueRequest{
		CommonName: req.CommonName, AltNames: req.AltNames, TTL: ttl,
	})
	if err != nil {
		writePKIError(w, err)
		return
	}
	writeData(w, map[string]any{
		"certificate":   issued.CertificatePEM,
		"private_key":   issued.PrivateKeyPEM,
		"issuing_ca":    issued.IssuingCAPEM,
		"serial_number": issued.SerialNumber,
		"expiration":    issued.Expiration,
	})
}

func writePKIError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, pki.ErrNoCA), errors.Is(err, pki.ErrRoleNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, pki.ErrCAExists), errors.Is(err, pki.ErrNotAllowed),
		errors.Is(err, pki.ErrInvalidName), errors.Is(err, pki.ErrInvalidKey):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
