// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// TestOperatorMetricsStoreStatus_Format checks the output of the status: the
// retention in days when it is whole days.
func TestOperatorMetricsStoreStatus_Format(t *testing.T) {
	ci.Parallel(t)

	out := formatMetricsStoreStatus(&api.MetricsStoreStatus{
		Server: "s1", Enabled: true, Retention: 7 * 24 * time.Hour, SampleInterval: 30 * time.Second,
		DiskBytes: 1 << 20, MaxDiskSize: 2 << 30, Series: 10, MaxSeries: 50000,
	})
	must.StrContains(t, out, "Retention         = 7d")
	must.StrContains(t, out, "Disk              = 1.0 MiB of 2.0 GiB")
	must.Eq(t, "1h30m0s", formatMetricsRetention(90*time.Minute))
	must.StrContains(t, formatMetricsStoreStatus(&api.MetricsStoreStatus{Server: "s1"}), "disabled")
}
