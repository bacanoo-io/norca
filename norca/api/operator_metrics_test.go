// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api/internal/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestOperator_MetricsSummary(t *testing.T) {
	testutil.Parallel(t)

	c, s := makeClient(t, nil, func(c *testutil.TestServerConfig) {
		c.DevMode = true
	})
	defer s.Stop()

	operator := c.Operator()
	qo := &QueryOptions{
		Params: map[string]string{
			"pretty": "1",
		},
	}

	// The summary is the last finished interval of the in-memory sink
	// (10 seconds, aligned to the clock). When the agent starts just
	// before an interval ends, that interval holds only its first moments
	// and may have no counter or sample yet; the next one is complete.
	var metrics *MetricsSummary
	var qm *QueryMeta
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			var err error
			metrics, qm, err = operator.MetricsSummary(qo)
			switch {
			case err != nil:
				return err
			case len(metrics.Gauges) == 0:
				return errors.New("no gauge yet")
			case len(metrics.Counters) == 0:
				return errors.New("no counter yet")
			case len(metrics.Samples) == 0:
				return errors.New("no sample yet")
			}
			return nil
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(500*time.Millisecond),
	))
	must.NotNil(t, qm)
	must.NotNil(t, metrics.Timestamp)  // should always get a TimeStamp
	must.SliceEmpty(t, metrics.Points) // may not have points yet
}

func TestOperator_Metrics_Prometheus(t *testing.T) {
	testutil.Parallel(t)

	c, s := makeClient(t, nil, func(c *testutil.TestServerConfig) {
		c.DevMode = true
		c.Telemetry = &testutil.Telemetry{PrometheusMetrics: true}
	})
	defer s.Stop()

	operator := c.Operator()
	qo := &QueryOptions{
		Params: map[string]string{
			"format": "prometheus",
		},
	}

	metrics, err := operator.Metrics(qo)
	must.NoError(t, err)
	must.NotNil(t, metrics)
	metricString := string(metrics[:])
	must.StrContains(t, metricString, "# HELP")
}
