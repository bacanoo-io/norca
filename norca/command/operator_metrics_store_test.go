// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// TestOperatorMetricsStoreStatus_Format checks the output of the status: the
// retention in days when it is whole days, and the hosts store.
func TestOperatorMetricsStoreStatus_Format(t *testing.T) {
	ci.Parallel(t)

	out := formatMetricsStoreStatus(&api.MetricsStoreStatus{
		Server: "s1", Enabled: true, Retention: 7 * 24 * time.Hour, SampleInterval: 30 * time.Second,
		DiskBytes: 1 << 20, MaxDiskSize: 2 << 30, Series: 10, MaxSeries: 50000,
		Hosts: &api.MetricsStoreDatabaseStatus{Retention: 36 * time.Hour, DiskBytes: 2 << 20, MaxDiskSize: 1 << 30,
			Series: 4, Partitions: 3, DiskPartitions: 1},
	})
	must.StrContains(t, out, "Retention         = 7d")
	must.StrContains(t, out, "Disk              = 1.0 MiB of 2.0 GiB")
	_, hosts, ok := strings.Cut(out, "Hosts Store")
	must.True(t, ok)
	must.StrContains(t, hosts, "Retention    = 36h0m0s")
	must.StrContains(t, hosts, "Disk         = 2.0 MiB of 1.0 GiB")
	must.StrContains(t, hosts, "Series       = 4")
	must.StrContains(t, hosts, "Partitions   = 3 (1 on disk)")
	must.StrContains(t, hosts, "Oldest Point = <none>")
	must.Eq(t, "1h30m0s", formatMetricsRetention(90*time.Minute))
	must.StrContains(t, formatMetricsStoreStatus(&api.MetricsStoreStatus{Server: "s1"}), "disabled")
}
