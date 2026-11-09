// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// window is a window of spec.schedule.
func window(start, length string, bounds map[string]float64) map[string]any {
	w := map[string]any{"start": start}
	if strings.Contains(length, " ") {
		w["end"] = length
	} else {
		w["duration"] = length
	}
	for k, v := range bounds {
		w[k] = v
	}
	return w
}

func TestNodeGroupBounds(t *testing.T) {
	ci.Parallel(t)

	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 30, 0, 0, time.UTC) } // 9 is a Monday
	day := window("0 7 * * 1-5", "12h", map[string]float64{"min": 4})
	night := window("0 22 * * *", "8h", map[string]float64{"max": 2})
	nightEnd := window("0 22 * * *", "0 6 * * *", map[string]float64{"max": 2})

	cases := []struct {
		name     string
		windows  map[string]any
		now      time.Time
		min, max int64
		windowed string
		err      string
	}{
		{name: "no schedule", now: at(9, 10), min: 1, max: 8},
		{name: "weekday window", windows: map[string]any{"day": day}, now: at(9, 10), min: 4, max: 8, windowed: `schedule "day": min 4`},
		{name: "after the window", windows: map[string]any{"day": day}, now: at(9, 20), min: 1, max: 8},
		{name: "weekend", windows: map[string]any{"day": day}, now: at(14, 10), min: 1, max: 8},
		{name: "night", windows: map[string]any{"night": night}, now: at(9, 23), min: 1, max: 2, windowed: `schedule "night": max 2`},
		{name: "night after midnight", windows: map[string]any{"night": night}, now: at(10, 3), min: 1, max: 2, windowed: `schedule "night": max 2`},
		{name: "night with end", windows: map[string]any{"night": nightEnd}, now: at(10, 3), min: 1, max: 2, windowed: `schedule "night": max 2`},
		{name: "night with end, morning", windows: map[string]any{"night": nightEnd}, now: at(10, 7), min: 1, max: 8},
		{
			name: "the larger min holds",
			windows: map[string]any{
				"a": window("0 * * * *", "1h", map[string]float64{"min": 3}),
				"b": window("0 * * * *", "1h", map[string]float64{"min": 5}),
			},
			now: at(9, 10), min: 5, max: 8, windowed: `schedule "a": min 3; schedule "b": min 5`,
		},
		{
			name: "a max below a min: the max holds",
			windows: map[string]any{
				"busy":  window("0 * * * *", "1h", map[string]float64{"min": 4}),
				"quiet": window("0 * * * *", "1h", map[string]float64{"max": 2}),
			},
			now: at(9, 10), min: 2, max: 2,
			windowed: `schedule "busy": min 4; schedule "quiet": max 2; the minimum 4 is above the maximum 2, the maximum holds`,
		},
		{name: "a duration in days", windows: map[string]any{"w": window("0 0 9 11 *", "2d", map[string]float64{"min": 3})}, now: at(10, 23), min: 3, max: 8, windowed: `schedule "w": min 3`},
		{
			name: "a bad window is left out, the others hold",
			windows: map[string]any{
				"bad":  window("not a cron x y", "1h", map[string]float64{"min": 6}),
				"good": window("0 * * * *", "1h", map[string]float64{"min": 3}),
			},
			now: at(9, 10), min: 3, max: 8, windowed: `schedule "good": min 3`, err: `schedule "bad": start:`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]any{"min": float64(1), "max": float64(8)}
			if tc.windows != nil {
				spec["schedule"] = tc.windows
			}
			b, err := nodeGroupBounds(spec, tc.now)
			if tc.err != "" {
				must.ErrorContains(t, err, tc.err)
			} else {
				must.NoError(t, err)
			}
			must.Eq(t, tc.min, b.min)
			must.Eq(t, tc.max, b.max)
			must.Eq(t, tc.windowed, b.windows)
		})
	}
}

// always is a window that is active whenever it is looked at.
func always(bounds map[string]float64) map[string]any {
	return map[string]any{"now": window("* * * * *", "1h", bounds)}
}

// A group below the minimum of an active window is raised to it, without
// blocked work and without a check.
func TestSchedule_RaisesToTheWindowMinimum(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	h, objects, _ := demandHarness(t, []testGroup{{
		name: "workers-a", dc: "dc-a",
		spec: map[string]any{"replicas": float64(1), "min": float64(1), "schedule": always(map[string]float64{"min": 3})},
	}}, nil)
	gauges(h, 1, 10)

	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-a": 3}, scaledTo(objects))
	reqs := objects.requests()
	must.StrContains(t, reqs[0].Message, `minimum 3 (schedule "now": min 3)`)
}

// A group above the maximum of an active window is lowered a node at a time,
// even when it is busy and has no scale_down block, and not while a node is
// still going.
func TestSchedule_LowersToTheWindowMaximum(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	group := testGroup{
		name: "workers-a", dc: "dc-a",
		spec:   map[string]any{"replicas": float64(3), "min": float64(0), "schedule": always(map[string]float64{"max": 1})},
		status: map[string]any{"replicas": float64(3)},
	}
	h, objects, _ := demandHarness(t, []testGroup{group}, nil)
	gauges(h, 3, 95) // busy: a scale down by use would not start

	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-a": 2}, scaledTo(objects))
	must.StrContains(t, objects.requests()[0].Message, `maximum 1 (schedule "now": max 1)`)

	// The operator has not removed the node yet (status.replicas 3, spec 2).
	h.clock.Advance(time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Len(t, 1, objects.requests())
}

// The checks of a group are capped by the maximum of an active window, so a
// check and the window do not undo each other.
func TestSchedule_CapsTheChecks(t *testing.T) {
	ci.Parallel(t)
	spec := nodeGroupSpec()
	spec["enforce"] = true
	spec["schedule"] = always(map[string]float64{"max": 3})
	h, objects := nodeGroupHarness(t, spec)

	must.NoError(t, evaluateNodeGroup(t, h))
	reqs := objects.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(3), reqs[0].Replicas) // the check asks for 4
}
