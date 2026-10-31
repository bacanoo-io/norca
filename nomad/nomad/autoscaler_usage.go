// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"time"

	"github.com/hashicorp/nomad/nomad/autoscaler"
)

// autoscalerUsageInterval is the task group usage aggregation window the
// heartbeat response asks clients to use: that of the built-in autoscaler,
// or the default while only the metrics store needs the usage. It is zero,
// so clients neither collect nor send usage, while neither needs it, the
// servers run no system bus (the reports travel on it only) or not every
// server is recent enough.
func (s *Server) autoscalerUsageInterval() time.Duration {
	if s.bus == nil {
		return 0
	}
	var interval time.Duration
	if s.autoscaler != nil {
		interval = s.autoscaler.UsageInterval()
	}
	if interval == 0 && s.metricsStore != nil {
		interval = autoscaler.DefaultClientMetricsInterval
	}
	if interval == 0 {
		return 0
	}
	if !ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false) {
		return 0
	}
	return interval
}
