package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/kv"
)

// kvWriteRequest is the body of a KV write. options is accepted (for Vault
// client compatibility) but currently ignored.
type kvWriteRequest struct {
	Data    map[string]any `json:"data"`
	Options map[string]any `json:"options"`
}

// versionsRequest is the body of the delete/undelete/destroy endpoints.
type versionsRequest struct {
	Versions []int `json:"versions"`
}

func (h *Handler) kvWrite(w http.ResponseWriter, r *http.Request) {
	var req kvWriteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	vm, err := h.kv.Write(r.Context(), r.PathValue("path"), req.Data)
	if err != nil {
		writeKVError(w, err)
		return
	}
	writeData(w, vm)
}

func (h *Handler) kvRead(w http.ResponseWriter, r *http.Request) {
	version, err := parseVersion(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "version must be an integer")
		return
	}
	data, vm, err := h.kv.Read(r.Context(), r.PathValue("path"), version)
	if err != nil {
		writeKVError(w, err)
		return
	}
	writeData(w, map[string]any{"data": data, "metadata": vm})
}

func (h *Handler) kvDeleteLatest(w http.ResponseWriter, r *http.Request) {
	if err := h.kv.Delete(r.Context(), r.PathValue("path")); err != nil {
		writeKVError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) kvDeleteVersions(w http.ResponseWriter, r *http.Request) {
	h.mutateVersions(w, r, h.kv.Delete)
}

func (h *Handler) kvUndelete(w http.ResponseWriter, r *http.Request) {
	h.mutateVersions(w, r, h.kv.Undelete)
}

func (h *Handler) kvDestroy(w http.ResponseWriter, r *http.Request) {
	h.mutateVersions(w, r, h.kv.Destroy)
}

// mutateVersions decodes a {"versions":[...]} body and applies op.
func (h *Handler) mutateVersions(w http.ResponseWriter, r *http.Request, op func(ctx context.Context, path string, versions ...int) error) {
	var req versionsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := op(r.Context(), r.PathValue("path"), req.Versions...); err != nil {
		writeKVError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) kvReadMetadata(w http.ResponseWriter, r *http.Request) {
	meta, err := h.kv.ReadMetadata(r.Context(), r.PathValue("path"))
	if err != nil {
		writeKVError(w, err)
		return
	}
	writeData(w, meta)
}

func (h *Handler) kvList(w http.ResponseWriter, r *http.Request) {
	keys, err := h.kv.List(r.Context(), r.PathValue("path"))
	if err != nil {
		writeKVError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": keys})
}

func (h *Handler) kvDeleteMetadata(w http.ResponseWriter, r *http.Request) {
	if err := h.kv.DeleteMetadata(r.Context(), r.PathValue("path")); err != nil {
		writeKVError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseVersion reads the ?version= query parameter (default 0 = latest).
func parseVersion(r *http.Request) (int, error) {
	q := r.URL.Query().Get("version")
	if q == "" {
		return 0, nil
	}
	return strconv.Atoi(q)
}

// writeData wraps v in the Vault-style {"data": ...} envelope.
func writeData(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusOK, map[string]any{"data": v})
}

// writeKVError maps engine/barrier errors to HTTP status codes.
func writeKVError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, kv.ErrSecretNotFound), errors.Is(err, kv.ErrVersionNotFound),
		errors.Is(err, kv.ErrVersionDeleted), errors.Is(err, kv.ErrVersionDestroyed):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, kv.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
