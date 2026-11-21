// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package sdk

import (
	"math"
	"testing"

	"github.com/shoenig/test/must"
)

func TestCountFromFloat(t *testing.T) {
	must.Eq(t, int64(3), CountFromFloat(3.9))
	must.Eq(t, int64(-3), CountFromFloat(-3.9))
	must.Eq(t, int64(math.MaxInt64), CountFromFloat(math.Inf(1)))
	must.Eq(t, int64(math.MaxInt64), CountFromFloat(1e19))
	must.Eq(t, int64(math.MinInt64), CountFromFloat(math.Inf(-1)))
	must.Eq(t, int64(0), CountFromFloat(math.NaN()))
}

func TestAddSubCount(t *testing.T) {
	must.Eq(t, int64(5), AddCount(2, 3))
	must.Eq(t, int64(math.MaxInt64), AddCount(2, math.MaxInt64))
	must.Eq(t, int64(math.MinInt64), AddCount(-2, math.MinInt64))
	must.Eq(t, int64(-1), SubCount(2, 3))
	must.Eq(t, int64(math.MinInt64), SubCount(-2, math.MaxInt64))
	must.Eq(t, int64(math.MaxInt64), SubCount(2, math.MinInt64))
}

// A dry run is told by the meta of the action, not by its count (review
// finding ASP-3): a count of -1 is capped like any other.
func TestScalingAction_DryRunByMeta(t *testing.T) {
	a := ScalingAction{Count: -1}
	must.False(t, a.IsDryRun())
	a.CapCount(1, 10)
	must.Eq(t, int64(1), a.Count)

	d := ScalingAction{Count: 5}
	d.SetDryRun()
	must.True(t, d.IsDryRun())
	d.CapCount(1, 3)
	must.Eq(t, int64(StrategyActionMetaValueDryRunCount), d.Count)
}
