// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestNodeScalingKinds_Schedule checks the schema and the rules of the
// windows of spec.schedule.
func TestNodeScalingKinds_Schedule(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		snap, err := s.State().Snapshot()
		if err != nil {
			return false
		}
		g, _ := snap.ObjectKindByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind)
		return g != nil
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	// The group: min 1, max 10.
	dry := func(windows map[string]any) error {
		spec := validNodeGroupSpec()
		spec["min"] = 1
		spec["schedule"] = windows
		_, err := c.apply(nodeGroupObject("g", spec), nil, true, "")
		return err
	}
	w := func(fields map[string]any) map[string]any { return map[string]any{"w": fields} }

	valid := map[string]map[string]any{
		"a minimum with a duration": w(map[string]any{"start": "0 7 * * 1-5", "duration": "12h", "min": 4}),
		"a maximum with an end":     w(map[string]any{"start": "0 22 * * *", "end": "0 6 * * *", "max": 2}),
		"both, in days":             w(map[string]any{"start": "0 0 1 * *", "duration": "2d", "min": 2, "max": 5}),
		"the bounds of the group":   w(map[string]any{"start": "0 0 * * *", "duration": "1h", "min": 10, "max": 1}),
		"two windows": {
			"day":   map[string]any{"start": "0 7 * * *", "duration": "12h", "min": 4},
			"night": map[string]any{"start": "0 22 * * *", "duration": "8h", "max": 2},
		},
	}
	for name, windows := range valid {
		// "the bounds of the group" has min 10 above max 1 in one window:
		// that is refused below; here its two halves are tried apart.
		if name == "the bounds of the group" {
			must.NoError(t, dry(w(map[string]any{"start": "0 0 * * *", "duration": "1h", "min": 10})), must.Sprint(name))
			must.NoError(t, dry(w(map[string]any{"start": "0 0 * * *", "duration": "1h", "max": 1})), must.Sprint(name))
			continue
		}
		must.NoError(t, dry(windows), must.Sprint(name))
	}

	invalid := map[string]map[string]any{
		"no start":                  w(map[string]any{"duration": "1h", "min": 2}),
		"a start of four fields":    w(map[string]any{"start": "0 7 * *", "duration": "1h", "min": 2}),
		"an end and a duration":     w(map[string]any{"start": "0 7 * * *", "end": "0 8 * * *", "duration": "1h", "min": 2}),
		"neither end nor duration":  w(map[string]any{"start": "0 7 * * *", "min": 2}),
		"neither min nor max":       w(map[string]any{"start": "0 7 * * *", "duration": "1h"}),
		"a min above its max":       w(map[string]any{"start": "0 7 * * *", "duration": "1h", "min": 4, "max": 3}),
		"a min above the max":       w(map[string]any{"start": "0 7 * * *", "duration": "1h", "min": 11}),
		"a max below the min":       w(map[string]any{"start": "0 7 * * *", "duration": "1h", "max": 0}),
		"a fractional duration":     w(map[string]any{"start": "0 7 * * *", "duration": "1.5h", "min": 2}),
		"a negative min":            w(map[string]any{"start": "0 7 * * *", "duration": "1h", "min": -1}),
		"an unknown field":          w(map[string]any{"start": "0 7 * * *", "duration": "1h", "min": 2, "timezone": "CET"}),
		"a window that is a string": {"w": "0 7 * * *"},
	}
	for name, windows := range invalid {
		must.Error(t, dry(windows), must.Sprint(name))
	}
}
