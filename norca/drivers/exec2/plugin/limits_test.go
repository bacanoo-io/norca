// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestResolvePidsLimit(t *testing.T) {
	cases := []struct {
		name         string
		plugin, task int64
		exp          int64
		err          bool
	}{
		{name: "none", exp: 0},
		{name: "plugin only", plugin: 100, exp: 100},
		{name: "task only", task: 50, exp: 50},
		{name: "task lower", plugin: 100, task: 50, exp: 50},
		{name: "task equal", plugin: 100, task: 100, exp: 100},
		{name: "task higher", plugin: 100, task: 200, err: true},
		{name: "negative", task: -1, err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePidsLimit(tc.plugin, tc.task)
			if tc.err {
				must.Error(t, err)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tc.exp, got)
		})
	}
}
