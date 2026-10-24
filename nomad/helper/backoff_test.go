// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package helper

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/shoenig/test/must"
)

func TestBackoff(t *testing.T) {
	cases := []struct {
		name    string
		base    time.Duration
		limit   time.Duration
		attempt uint64
		expect  time.Duration
	}{
		{name: "first attempt returns base", base: time.Second, limit: time.Minute, attempt: 0, expect: time.Second},
		{name: "second attempt doubles", base: time.Second, limit: time.Minute, attempt: 1, expect: 2 * time.Second},
		{name: "third attempt quadruples", base: time.Second, limit: time.Minute, attempt: 2, expect: 4 * time.Second},
		{name: "below limit", base: time.Second, limit: time.Minute, attempt: 5, expect: 32 * time.Second},
		{name: "capped at limit", base: time.Second, limit: time.Minute, attempt: 6, expect: time.Minute},
		{name: "reaches limit exactly", base: time.Second, limit: 8 * time.Second, attempt: 3, expect: 8 * time.Second},
		{name: "past exact limit", base: time.Second, limit: 8 * time.Second, attempt: 4, expect: 8 * time.Second},
		{name: "odd limit", base: 3, limit: 7, attempt: 1, expect: 6},
		{name: "odd limit capped", base: 3, limit: 7, attempt: 2, expect: 7},
		{name: "attempt where naive shift overflows", base: time.Second, limit: time.Minute, attempt: 34, expect: time.Minute},
		{name: "attempt 63", base: time.Nanosecond, limit: time.Hour, attempt: 63, expect: time.Hour},
		{name: "attempt 64", base: time.Nanosecond, limit: time.Hour, attempt: 64, expect: time.Hour},
		{name: "huge attempt", base: 50 * time.Millisecond, limit: time.Minute, attempt: 1 << 40, expect: time.Minute},
		{name: "max attempt", base: time.Second, limit: time.Minute, attempt: math.MaxUint64, expect: time.Minute},
		{name: "max duration limit", base: time.Second, limit: math.MaxInt64, attempt: math.MaxUint64, expect: math.MaxInt64},
		{name: "max duration limit, large attempt below cap", base: 1, limit: math.MaxInt64, attempt: 62, expect: 1 << 62},
		{name: "zero base", base: 0, limit: time.Minute, attempt: 0, expect: 0},
		{name: "zero base many attempts", base: 0, limit: time.Minute, attempt: math.MaxUint64, expect: 0},
		{name: "negative base", base: -time.Second, limit: time.Minute, attempt: 3, expect: 0},
		{name: "base equals limit", base: time.Minute, limit: time.Minute, attempt: 0, expect: time.Minute},
		{name: "base above limit", base: time.Hour, limit: time.Minute, attempt: 0, expect: time.Minute},
		{name: "base above limit, later attempt", base: time.Hour, limit: time.Minute, attempt: 10, expect: time.Minute},
		{name: "zero limit", base: time.Second, limit: 0, attempt: 2, expect: 0},
		{name: "negative limit", base: time.Second, limit: -time.Second, attempt: 2, expect: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Backoff(tc.base, tc.limit, tc.attempt)
			must.Eq(t, tc.expect, got)
		})
	}
}

// TestBackoff_Monotonic checks that over a long run of attempts the backoff
// never decreases, never goes negative and never exceeds the limit, which is
// the failure mode of a naive implementation that shifts before capping.
func TestBackoff_Monotonic(t *testing.T) {
	limits := []time.Duration{time.Second, time.Minute, time.Hour, math.MaxInt64}
	for _, limit := range limits {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			prev := time.Duration(0)
			for attempt := uint64(0); attempt < 200; attempt++ {
				got := Backoff(time.Millisecond, limit, attempt)
				must.Positive(t, got)
				must.LessEq(t, limit, got)
				must.GreaterEq(t, prev, got)
				prev = got
			}
			must.Eq(t, limit, prev)
		})
	}
}
