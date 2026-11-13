// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package resources

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestParseCPUStat(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name           string
		content        string
		expectedUser   time.Duration
		expectedSystem time.Duration
		expectedOK     bool
	}{
		{
			name:           "the file of a cgroup without the cpu controller",
			content:        "usage_usec 3500000\nuser_usec 2000000\nsystem_usec 1500000\n",
			expectedUser:   2 * time.Second,
			expectedSystem: 1500 * time.Millisecond,
			expectedOK:     true,
		},
		{
			name: "the file with the cpu controller",
			content: "usage_usec 12\nuser_usec 7\nsystem_usec 5\ncore_sched.force_idle_usec 0\n" +
				"nr_periods 0\nnr_throttled 0\nthrottled_usec 0\nnr_bursts 0\nburst_usec 0\n",
			expectedUser:   7 * time.Microsecond,
			expectedSystem: 5 * time.Microsecond,
			expectedOK:     true,
		},
		{
			name:       "an empty file",
			content:    "",
			expectedOK: false,
		},
		{
			name:         "only one of the two",
			content:      "usage_usec 12\nuser_usec 7\n",
			expectedUser: 7 * time.Microsecond,
			expectedOK:   false,
		},
		{
			name:           "a value that is not a number is not read",
			content:        "user_usec seven\nsystem_usec 5\n",
			expectedSystem: 5 * time.Microsecond,
			expectedOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, system, ok := parseCPUStat(bufio.NewScanner(strings.NewReader(tt.content)))
			must.Eq(t, tt.expectedOK, ok)
			must.Eq(t, tt.expectedUser, user)
			must.Eq(t, tt.expectedSystem, system)
		})
	}
}

// A containment without a cgroup accounts for nothing.
func TestContainment_CPUTime_NoCgroup(t *testing.T) {
	ci.Parallel(t)

	c := &containment{}
	_, _, ok := c.CPUTime()
	must.False(t, ok)
}
