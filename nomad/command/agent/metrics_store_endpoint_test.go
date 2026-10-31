// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestParseMetricsTime(t *testing.T) {
	ci.Parallel(t)

	zero, err := parseMetricsTime("")
	must.NoError(t, err)
	must.True(t, zero.IsZero())

	ts, err := parseMetricsTime("2026-10-31T10:00:00Z")
	must.NoError(t, err)
	must.Eq(t, time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC), ts)

	ts, err = parseMetricsTime("1761904800")
	must.NoError(t, err)
	must.Eq(t, int64(1761904800), ts.Unix())

	ts, err = parseMetricsTime("1h")
	must.NoError(t, err)
	must.Less(t, 5*time.Second, time.Since(ts)-time.Hour)

	_, err = parseMetricsTime("yesterday")
	must.ErrorContains(t, err, "not an RFC 3339 time")
}

// TestHTTP_MetricsStore checks the routes against a server without the
// store (the default of test agents).
func TestHTTP_MetricsStore(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		req, err := http.NewRequest(http.MethodGet, "/v1/metrics/history?job=api&group=web&from=1h&max_points=10", nil)
		must.NoError(t, err)
		obj, err := s.Server.MetricsHistoryRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		history := obj.(structs.MetricsHistoryResponse)
		must.False(t, history.Enabled)

		req, err = http.NewRequest(http.MethodGet, "/v1/metrics/history?job=api&group=web&max_points=-1", nil)
		must.NoError(t, err)
		_, err = s.Server.MetricsHistoryRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "invalid max_points")

		req, err = http.NewRequest(http.MethodGet, "/v1/operator/metrics-store", nil)
		must.NoError(t, err)
		obj, err = s.Server.OperatorMetricsStoreStatus(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.False(t, obj.(*structs.MetricsStoreStatus).Enabled)
	})
}
