// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// autoscalerUsageInterval is the task group usage aggregation window the
// heartbeat response asks clients to use. It is zero, so clients neither
// collect nor send usage, while the built-in autoscaler is disabled or not
// every server can accept usage only Node.UpdateAlloc requests.
func (s *Server) autoscalerUsageInterval() time.Duration {
	if s.autoscaler == nil {
		return 0
	}
	interval := s.autoscaler.UsageInterval()
	if interval == 0 {
		return 0
	}
	if !ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false) {
		return 0
	}
	return interval
}

// autoscalerRecordUsage hands the task group usage of a Node.UpdateAlloc
// request to the built-in autoscaler. It runs on the leader after the
// request was forwarded; the usage is kept in memory only and never written
// to Raft. Reports are accepted only from the node they describe.
func (s *Server) autoscalerRecordUsage(args *structs.AllocUpdateRequest) {
	report := args.TaskGroupUsage
	if report == nil || s.autoscaler == nil || report.NodeID == "" {
		return
	}

	if identity := args.GetIdentity(); identity == nil || identity.ClientID != report.NodeID {
		// Without ACLs the node is not resolved by Authenticate; verify its
		// secret here.
		node, err := s.State().NodeByID(nil, report.NodeID)
		if err != nil || node == nil || node.SecretID != args.AuthToken {
			s.logger.Debug("ignoring task group usage from an unknown node", "node_id", report.NodeID)
			return
		}
	}

	s.autoscaler.RecordUsage(report)
}
