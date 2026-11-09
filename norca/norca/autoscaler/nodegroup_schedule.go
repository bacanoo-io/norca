// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
)

// The scheduled windows of a node group. spec.schedule names recurring
// windows (a cron start, and an end or a duration, in UTC); while a window is
// active its min raises the minimum of the group and its max lowers the
// maximum. Overlapping windows: the largest min and the smallest max hold. The
// bounds hold for every decision: the checks, the blocked work, the headroom
// and the scale down; a group below the minimum of a window is raised to it,
// one above the maximum is lowered a node at a time.

// groupBounds are the minimum and maximum of a node group at a time.
type groupBounds struct {
	min, max int64
	// windows describes the active windows that changed the bounds, for the
	// reasons of the decisions ("" when none did).
	windows string
}

// nodeGroupBounds returns the bounds of a group at now: spec.min and spec.max,
// changed by the active windows of spec.schedule. A window that cannot be
// read is left out and reported in the error; the others still apply. When
// the windows ask for a minimum above the maximum, the maximum holds.
func nodeGroupBounds(spec map[string]any, now time.Time) (groupBounds, error) {
	b := groupBounds{min: asInt(spec["min"]), max: asInt(spec["max"])}
	windows := asMap(spec["schedule"])
	if len(windows) == 0 {
		return b, nil
	}
	names := make([]string, 0, len(windows))
	for name := range windows {
		names = append(names, name)
	}
	sort.Strings(names)

	var active, bad []string
	for _, name := range names {
		w := asMap(windows[name])
		on, err := windowActive(w, now)
		if err != nil {
			bad = append(bad, fmt.Sprintf("schedule %q: %v", name, err))
			continue
		}
		if !on {
			continue
		}
		var parts []string
		if v, ok := w["min"]; ok {
			if m := asInt(v); m > b.min {
				b.min = m
			}
			parts = append(parts, fmt.Sprintf("min %d", asInt(v)))
		}
		if v, ok := w["max"]; ok {
			if m := asInt(v); m < b.max {
				b.max = m
			}
			parts = append(parts, fmt.Sprintf("max %d", asInt(v)))
		}
		active = append(active, fmt.Sprintf("schedule %q: %s", name, strings.Join(parts, ", ")))
	}
	if b.min > b.max {
		active = append(active, fmt.Sprintf("the minimum %d is above the maximum %d, the maximum holds", b.min, b.max))
		b.min = b.max
	}
	b.windows = strings.Join(active, "; ")
	if len(bad) > 0 {
		return b, fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return b, nil
}

// windowActive tells whether a window of spec.schedule is active at now.
func windowActive(w map[string]any, now time.Time) (bool, error) {
	s := &sdk.ScalingPolicySchedule{Start: asString(w["start"]), End: asString(w["end"])}
	if d := asString(w["duration"]); d != "" {
		dur, err := parseNodeDuration(d)
		if err != nil {
			return false, fmt.Errorf("duration: %w", err)
		}
		s.Duration = dur.String()
	}
	if (s.End == "") == (s.Duration == "") {
		return false, fmt.Errorf("exactly one of end and duration must be set")
	}
	c, err := compileSchedule(s)
	if err != nil {
		return false, err
	}
	return c.activeAt(now), nil
}
