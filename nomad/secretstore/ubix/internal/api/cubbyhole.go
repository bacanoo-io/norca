package api

import (
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/cubbyhole"
)

// cubbyholeWriteRequest is the body of a cubbyhole write. Unlike KV v2 there is
// no {"data": ...} envelope or versioning: the JSON object is stored as-is.
type cubbyholeWriteRequest map[string]any

// The cubbyhole handlers scope every operation to the calling token, taken from
// the request context. There is no cross-token access path, so no ACL check can
// widen it — a token reaches only its own cubbyhole.

func (h *Handler) cubbyWrite(w http.ResponseWriter, r *http.Request) {
	tok, ok := tokenFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no token on request")
		return
	}
	var req cubbyholeWriteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.cubbyhole.Write(r.Context(), tok.ID, r.PathValue("path"), req); err != nil {
		writeCubbyholeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) cubbyRead(w http.ResponseWriter, r *http.Request) {
	tok, ok := tokenFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no token on request")
		return
	}
	data, err := h.cubbyhole.Read(r.Context(), tok.ID, r.PathValue("path"))
	if err != nil {
		writeCubbyholeError(w, err)
		return
	}
	writeData(w, data)
}

func (h *Handler) cubbyList(w http.ResponseWriter, r *http.Request) {
	tok, ok := tokenFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no token on request")
		return
	}
	keys, err := h.cubbyhole.List(r.Context(), tok.ID, r.PathValue("path"))
	if err != nil {
		writeCubbyholeError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": keys})
}

func (h *Handler) cubbyDelete(w http.ResponseWriter, r *http.Request) {
	tok, ok := tokenFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no token on request")
		return
	}
	if err := h.cubbyhole.Delete(r.Context(), tok.ID, r.PathValue("path")); err != nil {
		writeCubbyholeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeCubbyholeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, cubbyhole.ErrSecretNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, cubbyhole.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
