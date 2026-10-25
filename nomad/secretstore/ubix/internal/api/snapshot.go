package api

import "net/http"

// snapshot streams a backup of the encrypted store. The snapshot contains only
// ciphertext, but it is the entire dataset, so this is a privileged operation.
func (h *Handler) snapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="ubixvault.snapshot"`)
	if err := h.core.Snapshot(r.Context(), w); err != nil {
		// Headers may already be sent; log-and-truncate is the best we can do.
		// A well-behaved client detects truncation via the missing trailing data.
		writeInternal(w, err)
		return
	}
}
