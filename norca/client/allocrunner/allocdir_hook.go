// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"github.com/bacanoo-io/norca/norca/client/allocdir"
	log "github.com/hashicorp/go-hclog"
)

// allocDirHook creates and destroys the root directory and shared directories
// for an allocation.
type allocDirHook struct {
	allocDir *allocdir.AllocDir
	logger   log.Logger
}

func newAllocDirHook(logger log.Logger, allocDir *allocdir.AllocDir) *allocDirHook {
	ad := &allocDirHook{
		allocDir: allocDir,
	}
	ad.logger = logger.Named(ad.Name())
	return ad
}

func (h *allocDirHook) Name() string {
	return "alloc_dir"
}

func (h *allocDirHook) Prerun() error {
	return h.allocDir.Build()
}

func (h *allocDirHook) Destroy() error {
	return h.allocDir.Destroy()
}
