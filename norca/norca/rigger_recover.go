// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"
	"runtime/debug"

	"github.com/bacanoo-io/norca/norca/norca/structs"
	rigger "github.com/bacanoo-io/norca/rigger"
)

// recoverRigger, deferred by every endpoint of Rigger and by the goroutine
// of an operation, turns a panic into an error: the RPC connections of the
// servers have no recover, so a panic there would end the process of the
// leader. The engine recovers its own; this is for what is around it (the
// registries, the fetcher, the bundles). The text of the panic is not
// logged: it may hold a sensitive value.
func (s *Server) recoverRigger(op string, out *error) {
	if r := recover(); r != nil {
		s.logger.Named("rigger").Error("recovered a panic", "op", op, "stack", string(debug.Stack()))
		*out = structs.NewErrRPCCodedf(http.StatusInternalServerError, "%v", rigger.ErrInternal)
	}
}
