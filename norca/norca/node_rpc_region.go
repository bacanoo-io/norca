// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// checkNodeRPCRegion refuses a node RPC that would cross a region boundary:
// the RPCs that only client nodes call about themselves (registration,
// status, allocation updates and pulls, events, tokens and claims for their
// allocations) stay in the region of the node. A node registered in another
// region would attract placements there that it can never run.
//
// The endpoints call it before forwarding, so it runs on the first server the
// node reached and again on every server the request is forwarded to:
//
//   - A request that names another region than the server's is refused
//     instead of being forwarded there. Inside a region a node RPC is only
//     forwarded to the leader, with the region unchanged.
//   - With TLS and verify_server_hostname, the connection must carry a
//     client or server certificate of this region. The TLS handshake accepts
//     server certificates of every region for federation, so this refuses a
//     node RPC that a server of another region forwarded. It does not rely
//     on the Forwarded flag of the request, which the caller can set.
//
// Without verify_server_hostname the certificate names nothing reliable, and
// only the first rule applies.
func (s *Server) checkNodeRPCRegion(ctx *RPCContext, info structs.RPCInfo) error {
	// An empty region is refused by forward with its own error.
	if region := info.RequestRegion(); region != "" && region != s.Region() {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"node RPCs stay in their region: this server is in region %q, the request names %q",
			s.Region(), region)
	}
	if err := validateTLSCertificateLevel(s, ctx, tlsCertificateLevelClient); err != nil {
		return structs.NewErrRPCCodedf(http.StatusForbidden, "node RPCs stay in their region: %v", err)
	}
	return nil
}
