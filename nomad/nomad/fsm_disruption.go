// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"errors"
	"fmt"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/nomad/structs"
)

// applyAllocEvict evicts an allocation if its disruption budget allows it.
// The response is the error, or whether the allocation was evicted (false
// if it was already terminal or being migrated).
func (n *nomadFSM) applyAllocEvict(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "alloc_evict"}, time.Now())
	var req structs.AllocEvictRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	evicted, err := n.state.EvictAlloc(msgType, index, &req)
	if err != nil {
		if !errors.Is(err, structs.ErrDisruptionBudgetExceeded) {
			n.logger.Error("EvictAlloc failed", "error", err)
		}
		return err
	}

	if evicted && req.Eval != nil {
		n.handleUpsertedEvals([]*structs.Evaluation{req.Eval})
	}
	return evicted
}
