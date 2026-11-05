// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"errors"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

type gaugeCall struct {
	metric   string
	labels   map[string]string
	from, to time.Time
}

func fakeGauges(avg, max float64, samples int, calls *[]gaugeCall) func(string, map[string]string, time.Time, time.Time) (float64, float64, int, error) {
	return func(metric string, labels map[string]string, from, to time.Time) (float64, float64, int, error) {
		*calls = append(*calls, gaugeCall{metric, labels, from, to})
		return avg, max, samples, nil
	}
}

func TestClusterAPM_Query(t *testing.T) {
	ci.Parallel(t)
	now := time.Now()
	window := sdk.TimeRange{From: now.Add(-5 * time.Minute), To: now}

	t.Run("the scope comes from the target", func(t *testing.T) {
		var calls []gaugeCall
		apm := &clusterAPM{
			target:  map[string]string{TargetConfigKeyDatacenter: "dc-a", TargetConfigKeyPool: "workers", TargetConfigKeyNodeClass: "gpu"},
			history: fakeGauges(61.5, 80, 10, &calls),
		}
		got, err := apm.Query("cluster.cpu_allocated", window)
		must.NoError(t, err)
		must.Eq(t, sdk.TimestampedMetrics{{Timestamp: now, Value: 61.5}}, got)
		must.Len(t, 1, calls)
		must.Eq(t, "norca.cluster.cpu_allocated", calls[0].metric)
		must.Eq(t, map[string]string{"dc": "dc-a", "pool": "workers", "class": "gpu"}, calls[0].labels)
		must.Eq(t, window.From, calls[0].from)
	})

	t.Run("selectors in the query win, op=max reads the maximum", func(t *testing.T) {
		var calls []gaugeCall
		apm := &clusterAPM{
			target:  map[string]string{TargetConfigKeyDatacenter: "dc-a", TargetConfigKeyPool: "workers", TargetConfigKeyNodeClass: "gpu"},
			history: fakeGauges(61.5, 80, 10, &calls),
		}
		got, err := apm.Query("norca.cluster.nodes_ready?dc=dc-b&class=&op=max", window)
		must.NoError(t, err)
		must.Eq(t, 80.0, got[0].Value)
		must.Eq(t, map[string]string{"dc": "dc-b", "pool": "workers", "class": ""}, calls[0].labels)
	})

	t.Run("an instant query reads the latest intervals", func(t *testing.T) {
		var calls []gaugeCall
		apm := &clusterAPM{history: fakeGauges(10, 10, 1, &calls)}
		_, err := apm.Query("cluster.memory_allocated?dc=d&pool=p", sdk.TimeRange{From: now, To: now})
		must.NoError(t, err)
		must.Eq(t, now.Add(-clusterQueryWindow), calls[0].from)
	})

	t.Run("no samples is an error that names the series", func(t *testing.T) {
		var calls []gaugeCall
		apm := &clusterAPM{history: fakeGauges(0, 0, 0, &calls)}
		_, err := apm.Query("cluster.cpu_allocated?dc=d&pool=p&class=x", window)
		must.ErrorContains(t, err, "no samples")
		must.ErrorContains(t, err, `class="x"`)
	})

	t.Run("the history can fail", func(t *testing.T) {
		apm := &clusterAPM{history: func(string, map[string]string, time.Time, time.Time) (float64, float64, int, error) {
			return 0, 0, 0, errors.New("store closed")
		}}
		_, err := apm.Query("cluster.cpu_allocated", window)
		must.ErrorContains(t, err, "store closed")
	})

	t.Run("without a metrics store", func(t *testing.T) {
		_, err := (&clusterAPM{}).Query("cluster.cpu_allocated", window)
		must.ErrorContains(t, err, "metrics store is not available")
	})
}

func TestClusterAPM_BadQueries(t *testing.T) {
	ci.Parallel(t)
	apm := &clusterAPM{history: fakeGauges(1, 1, 1, new([]gaugeCall))}
	r := sdk.TimeRange{From: time.Now().Add(-time.Hour), To: time.Now()}
	for query, want := range map[string]string{
		"nomad.cpu":                       "not a cluster metric",
		"cluster.no_such_gauge":           "unknown cluster metric",
		"cluster.cpu_allocated?rack=r1":   "unknown selector",
		"cluster.cpu_allocated?op=median": "unknown op",
		"cluster.cpu_allocated?%zz":       "invalid selectors",
		"quota.cpu_mhz_used":              "not a cluster metric",
	} {
		_, err := apm.Query(query, r)
		must.ErrorContains(t, err, want, must.Sprintf("query %q", query))
	}
}

func TestClusterAPM_IsAKnownSource(t *testing.T) {
	ci.Parallel(t)
	must.SliceContains(t, (&Config{}).KnownSources(), plugins.InternalAPMNorca)
}

// A check with source "norca" is served from the cluster gauges: a job is
// scaled by the allocated share of a node pool, with no APM configured.
func TestAutoscaler_ClusterSource(t *testing.T) {
	ci.Parallel(t)
	doc := targetValuePolicy(nil)
	check := doc["check"].([]interface{})[0].(map[string]interface{})["cpu"].([]interface{})[0].(map[string]interface{})
	check["source"] = plugins.InternalAPMNorca
	check["query"] = "cluster.cpu_allocated?dc=dc1&pool=default"

	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, doc, nil)
	var calls []gaugeCall
	h.deps.GaugeHistory = fakeGauges(100, 100, 6, &calls)
	h.newRunner()

	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	// 2 running allocations at 100 of a target of 50.
	must.Eq(t, int64(4), *reqs[0].Count)
	must.Len(t, 1, calls)
	must.Eq(t, "norca.cluster.cpu_allocated", calls[0].metric)
	must.Eq(t, map[string]string{"dc": "dc1", "pool": "default", "class": ""}, calls[0].labels)
}

// The check is accepted by the admission of the policy: norca is a known source.
func TestAutoscaler_ClusterSourceNeedsNoConfiguration(t *testing.T) {
	ci.Parallel(t)
	cfg := DefaultConfig()
	must.SliceContains(t, cfg.KnownSources(), "norca")
}
