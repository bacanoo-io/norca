// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// parseTraceParent puts the traceparent of the request's span into the RPC
// arguments, so the servers continue the request's trace; it leaves the
// arguments as they are when the request has no span (no otlp block, an
// excluded route, a blocking query).
func parseTraceParent(req *http.Request, info *structs.InternalRpcInfo) {
	if tp := telemetry.TraceParentFromContext(req.Context()); tp != "" {
		info.SetTraceParent(tp)
	}
}
