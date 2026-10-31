// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"time"
)

// autoscalerUsageInterval is the task group usage aggregation window the
// heartbeat response asks clients to use. It is zero, so clients neither
// collect nor send usage, while the built-in autoscaler is disabled, the
// servers run no system bus (the reports travel on it only) or not every
// server is recent enough.
func (s *Server) autoscalerUsageInterval() time.Duration {
	if s.autoscaler == nil || s.bus == nil {
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
