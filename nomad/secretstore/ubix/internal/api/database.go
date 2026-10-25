package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/database"
)

type dbConfigRequest struct {
	ConnectionURL string `json:"connection_url"`
}

type dbRoleRequest struct {
	CreationStatements []string      `json:"creation_statements"`
	DefaultTTL         vaultDuration `json:"default_ttl"` // duration string, e.g. "1h"
}

func (h *Handler) dbConfigure(w http.ResponseWriter, r *http.Request) {
	var req dbConfigRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ConnectionURL == "" {
		writeError(w, http.StatusBadRequest, "connection_url is required")
		return
	}
	if err := h.database.Configure(r.Context(), req.ConnectionURL); err != nil {
		// A failed Configure is most likely a bad connection string or an
		// unreachable database — a client error.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) dbConfigStatus(w http.ResponseWriter, r *http.Request) {
	ok, err := h.database.Configured(r.Context())
	if err != nil {
		writeDatabaseError(w, err)
		return
	}
	writeData(w, map[string]any{"configured": ok})
}

func (h *Handler) dbWriteRole(w http.ResponseWriter, r *http.Request) {
	var req dbRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ttl, err := time.ParseDuration(string(req.DefaultTTL))
	if err != nil || ttl <= 0 {
		writeError(w, http.StatusBadRequest, "default_ttl must be a positive duration (e.g. \"1h\")")
		return
	}
	role := database.Role{CreationStatements: req.CreationStatements, DefaultTTL: ttl}
	if err := h.database.WriteRole(r.Context(), r.PathValue("name"), role); err != nil {
		writeDatabaseError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) dbReadRole(w http.ResponseWriter, r *http.Request) {
	role, err := h.database.ReadRole(r.Context(), r.PathValue("name"))
	if err != nil {
		writeDatabaseError(w, err)
		return
	}
	writeData(w, map[string]any{
		"creation_statements": role.CreationStatements,
		"default_ttl":         role.DefaultTTL.String(),
	})
}

func (h *Handler) dbListRoles(w http.ResponseWriter, r *http.Request) {
	names, err := h.database.ListRoles(r.Context())
	if err != nil {
		writeDatabaseError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": names})
}

func (h *Handler) dbDeleteRole(w http.ResponseWriter, r *http.Request) {
	if err := h.database.DeleteRole(r.Context(), r.PathValue("name")); err != nil {
		writeDatabaseError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dbCredentials issues a fresh short-lived database credential for a role. The
// lease is attributed to the requesting token so it can be revoked with it.
func (h *Handler) dbCredentials(w http.ResponseWriter, r *http.Request) {
	// Lease-count quota: refuse to issue a new lease when the matching quota is
	// already at its cap. Checked before issuance so no credential is created.
	_ = h.quotas.EnsureLoaded(r.Context())
	if lq, ok := h.quotas.MatchLeaseCount(apiPath(r)); ok {
		n, err := h.database.CountLeasesUnder(r.Context(), lq.Path)
		if err != nil {
			writeInternal(w, err)
			return
		}
		if n >= lq.MaxLeases {
			h.metrics.ObserveQuotaExceeded(lq.Name)
			writeError(w, http.StatusTooManyRequests, "lease-count quota exceeded: "+lq.Name)
			return
		}
	}

	var createdBy string
	if tok, ok := tokenFromContext(r.Context()); ok {
		createdBy = tok.ID
	}
	cred, err := h.database.GenerateCredentials(r.Context(), r.PathValue("name"), createdBy)
	if err != nil {
		writeDatabaseError(w, err)
		return
	}
	writeData(w, map[string]any{
		"lease_id":       cred.LeaseID,
		"lease_duration": int(cred.TTL.Seconds()),
		"username":       cred.Username,
		"password":       cred.Password,
	})
}

type leaseIDRequest struct {
	LeaseID   string        `json:"lease_id"`
	Increment vaultDuration `json:"increment"` // renew only; optional duration
}

func (h *Handler) leaseRevoke(w http.ResponseWriter, r *http.Request) {
	var req leaseIDRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.database.Revoke(r.Context(), req.LeaseID); err != nil {
		writeDatabaseError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) leaseRenew(w http.ResponseWriter, r *http.Request) {
	var req leaseIDRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var ttl time.Duration
	if req.Increment != "" {
		d, err := time.ParseDuration(string(req.Increment))
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "increment must be a positive duration")
			return
		}
		ttl = d
	}
	info, err := h.database.Renew(r.Context(), req.LeaseID, ttl)
	if err != nil {
		writeDatabaseError(w, err)
		return
	}
	writeData(w, map[string]any{"lease_id": info.LeaseID, "lease_duration": int(info.TTL.Seconds())})
}

func (h *Handler) leaseLookup(w http.ResponseWriter, r *http.Request) {
	var req leaseIDRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	info, err := h.database.Lookup(r.Context(), req.LeaseID)
	if err != nil {
		writeDatabaseError(w, err)
		return
	}
	writeData(w, map[string]any{
		"lease_id":       info.LeaseID,
		"role":           info.Role,
		"lease_duration": int(info.TTL.Seconds()),
	})
}

// tokenRevokeSelf revokes the calling token and cascades: every token it created
// (and theirs) is revoked first, and for each one its dynamic-database leases are
// revoked and its cubbyhole destroyed, so neither credentials nor private data
// outlive the token that owned them.
func (h *Handler) tokenRevokeSelf(w http.ResponseWriter, r *http.Request) {
	tok, ok := tokenFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no token on request")
		return
	}
	if _, err := h.tokens.RevokeTree(r.Context(), tok.ID, h.cleanupToken); err != nil {
		writeInternal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cleanupToken destroys what a token owned before its record goes: its
// dynamic-database leases and its cubbyhole.
func (h *Handler) cleanupToken(ctx context.Context, id string) error {
	if _, err := h.database.RevokeByToken(ctx, id); err != nil {
		return err
	}
	return h.cubbyhole.Destroy(ctx, id)
}

func writeDatabaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, database.ErrRoleNotFound), errors.Is(err, database.ErrLeaseNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, database.ErrNotConfigured), errors.Is(err, database.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
