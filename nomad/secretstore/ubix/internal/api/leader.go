package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/core"
)

// leaderResponse is Vault's sys/leader shape, minus the performance-standby and
// Raft fields this vault does not have.
type leaderResponse struct {
	HAEnabled            bool   `json:"ha_enabled"`
	IsSelf               bool   `json:"is_self"`
	ActiveTime           string `json:"active_time,omitempty"`
	LeaderAddress        string `json:"leader_address"`
	LeaderClusterAddress string `json:"leader_cluster_address"`
	PerformanceStandby   bool   `json:"performance_standby"`
}

// leader reports which replica is active (sys/leader). Unauthenticated, as in
// Vault, and answered by every replica itself.
func (h *Handler) leader(w http.ResponseWriter, r *http.Request) {
	if !h.core.HAEnabled() {
		writeJSON(w, http.StatusOK, leaderResponse{})
		return
	}
	info, err := h.core.Leader(r.Context())
	if err != nil {
		writeInternal(w, err)
		return
	}
	resp := leaderResponse{
		HAEnabled:            true,
		IsSelf:               info.IsSelf,
		LeaderAddress:        info.APIAddr,
		LeaderClusterAddress: info.ClusterAddr,
	}
	if t := h.core.ActiveSince(); info.IsSelf && !t.IsZero() {
		resp.ActiveTime = t.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

// stepDown makes the active replica give up the lock so another takes over
// (sys/step-down). A standby forwards it to the active replica like any other
// request, so it always lands where it can act.
func (h *Handler) stepDown(w http.ResponseWriter, _ *http.Request) {
	switch err := h.core.StepDown(); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, core.ErrHANotConfigured):
		writeError(w, http.StatusBadRequest, "HA is not enabled on this vault")
	case errors.Is(err, core.ErrNotActive):
		writeError(w, http.StatusServiceUnavailable, "this replica is not active; retry")
	default:
		writeInternal(w, err)
	}
}
