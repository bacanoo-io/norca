// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestUsageAPM_ScopeOf(t *testing.T) {
	ci.Parallel(t)

	apm := &usageAPM{target: targetKey{namespace: "default", job: "shop", group: "web"}}

	tests := []struct {
		name     string
		query    string
		expected valueScope
	}{
		{name: "built-in metric of the own group", query: MetricCPU, expected: valueScopeWindow},
		{name: "average of the own group", query: "taskgroup_avg_cpu-allocated", expected: valueScopeWindow},
		{name: "maximum of the own group", query: "taskgroup_max_memory", expected: valueScopeWindow},
		{name: "own group named in full", query: "taskgroup_avg_cpu-allocated/web/shop@default", expected: valueScopeWindow},
		{name: "a sum is a total", query: "taskgroup_sum_cpu-allocated", expected: valueScopeUnknown},
		{name: "another group", query: "nomad.cpu/api/shop@default", expected: valueScopeUnknown},
		{name: "another job", query: "taskgroup_avg_cpu-allocated/web/other@default", expected: valueScopeUnknown},
		{name: "a query that cannot be read", query: "taskgroup_p99_cpu", expected: valueScopeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, apm.scopeOf(tt.query))
		})
	}
}
