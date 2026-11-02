// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"context"

	"github.com/bacanoo-io/norca/norca/client/config"
	log "github.com/hashicorp/go-hclog"
)

// upstreamAllocsHook waits for a PrevAllocWatcher to exit before allowing
// an allocation to be executed
type upstreamAllocsHook struct {
	allocWatcher config.PrevAllocWatcher
	logger       log.Logger
}

func newUpstreamAllocsHook(logger log.Logger, allocWatcher config.PrevAllocWatcher) *upstreamAllocsHook {
	h := &upstreamAllocsHook{
		allocWatcher: allocWatcher,
	}
	h.logger = logger.Named(h.Name())
	return h
}

func (h *upstreamAllocsHook) Name() string {
	return "await_previous_allocations"
}

func (h *upstreamAllocsHook) Prerun() error {
	// Wait for a previous alloc - if any - to terminate
	return h.allocWatcher.Wait(context.Background())
}
