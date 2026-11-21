// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package prometheus

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/prometheus/common/model"
	"github.com/shoenig/test/must"
)

// A range query keeps under the number of points Prometheus answers (review
// finding ASP-6: a step of 1 second failed for a window over about 3 hours).
func TestQueryStep(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	for _, window := range []time.Duration{time.Minute, 3 * time.Hour, 4 * time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
		step := queryStep(sdk.TimeRange{From: at.Add(-window), To: at})
		points := int64(window/step) + 1
		must.LessEq(t, int64(maxQueryPoints), points, must.Sprintf("window %s, step %s", window, step))
		must.GreaterEq(t, time.Second, step)
		must.Eq(t, time.Duration(0), step%time.Second)
	}
	must.Eq(t, time.Second, queryStep(sdk.TimeRange{From: at.Add(-time.Hour), To: at}))
}

// An instant vector of several series is several streams, so Query refuses it
// as it refuses a matrix of several series (review finding ASP-5).
func TestParseVector_SeriesAreStreams(t *testing.T) {
	v := model.Vector{
		{Metric: model.Metric{"instance": "a"}, Value: 10, Timestamp: 1000},
		{Metric: model.Metric{"instance": "b"}, Value: 90, Timestamp: 1000},
	}
	streams, err := parseVector(v)
	must.NoError(t, err)
	must.Len(t, 2, streams)
	must.Len(t, 1, streams[0])

	one, err := parseVector(v[:1])
	must.NoError(t, err)
	must.Len(t, 1, one)
	must.Eq(t, 10.0, one[0][0].Value)
}
