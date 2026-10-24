// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// updatePolicy changes the scaling policy of the harness job.
func (h *harness) updatePolicy(fn func(p *structs.ScalingPolicy)) {
	job := h.job.Copy()
	fn(job.TaskGroups[0].Scaling)
	h.index++
	must.NoError(h.t, h.store.UpsertJob(structs.MsgTypeTestSetup, h.index, nil, job))
	h.job = job
	h.sync()
}

// typedTargetValue is a typed policy with one target-value check on the
// fake APM.
func typedTargetValue(p *structs.ScalingPolicy) {
	p.Policy = nil
	p.EvaluationInterval = 10 * time.Second
	p.Cooldown = time.Minute
	p.Checks = []*structs.ScalingCheck{{
		Name:           "cpu",
		Source:         "fake",
		Query:          "cpu",
		Strategy:       "target-value",
		StrategyConfig: map[string]string{"target": "50"},
	}}
}

// eventRecorder records the published Scaling events.
type eventRecorder struct {
	l      sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	typ string
	ev  *structs.ScalingDecisionEvent
}

func (r *eventRecorder) publish(typ string, ev *structs.ScalingDecisionEvent) {
	r.l.Lock()
	defer r.l.Unlock()
	r.events = append(r.events, recordedEvent{typ, ev})
}

func (r *eventRecorder) list() []recordedEvent {
	r.l.Lock()
	defer r.l.Unlock()
	return append([]recordedEvent(nil), r.events...)
}

func newEventHarness(t *testing.T) (*harness, *eventRecorder) {
	rec := &eventRecorder{}
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.deps.PublishEvent = rec.publish
	h.newRunner()
	return h, rec
}

func TestAutoscaler_TypedPolicy(t *testing.T) {
	ci.Parallel(t)
	h, rec := newEventHarness(t)
	h.updatePolicy(typedTargetValue)

	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(4), *reqs[0].Count)

	// The decision is published on the Scaling topic.
	events := rec.list()
	must.Len(t, 1, events)
	must.Eq(t, structs.TypeScalingDecision, events[0].typ)
	ev := events[0].ev
	must.Eq(t, h.job.ID, ev.JobID)
	must.Eq(t, "web", ev.Group)
	must.Eq(t, h.policyID(), ev.PolicyID)
	must.Eq(t, int64(2), ev.PreviousCount)
	must.Eq(t, int64(4), ev.Count)
	must.Eq(t, "up", ev.Direction)
	must.Eq(t, "cpu", ev.Check)
	must.NotEq(t, "", ev.Reason)
}

func TestAutoscaler_TypedMetricCheck(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.updatePolicy(func(p *structs.ScalingPolicy) {
		p.Policy = map[string]interface{}{
			"check": []interface{}{map[string]interface{}{"cpu": []interface{}{map[string]interface{}{
				"metric":   "nomad.cpu",
				"strategy": "target-value",
				"target":   70,
			}}}},
		}
		must.NoError(t, p.FillTypedPolicy())
	})

	// The built-in metric is served by the nomad-apm for now.
	c := h.runner.handles[h.policyID()].compiled.policy.Checks[0]
	must.Eq(t, "nomad-apm", c.Source)
	must.Eq(t, "taskgroup_avg_cpu-allocated/web/"+h.job.ID+"@default", c.Query)
	must.Eq(t, map[string]string{"target": "70"}, c.Strategy.Config)
}

func TestAutoscaler_LegacyDocumentFilledAtRuntime(t *testing.T) {
	ci.Parallel(t)

	// Policies stored before the typed policy existed only have the
	// document; the runtime derives the typed policy itself.
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(map[string]interface{}{
		"scale_during_deployment": "up_only",
	}), nil)
	c := h.runner.handles[h.policyID()].compiled
	must.NotNil(t, c)
	must.Eq(t, structs.ScaleDuringDeploymentUpOnly, c.scaleDuringDeployment)
	must.Eq(t, time.Minute, c.policy.Cooldown)
}

// startDeployment records an active deployment of the harness job.
func (h *harness) startDeployment(canaries bool) {
	d := structs.NewDeployment(h.job, 50)
	d.TaskGroups = map[string]*structs.DeploymentState{"web": {DesiredTotal: 2}}
	if canaries {
		d.TaskGroups["web"].DesiredCanaries = 1
	}
	h.index++
	must.NoError(h.t, h.store.UpsertDeployment(h.index, d))
}

func TestAutoscaler_NoScalingDuringDeployment(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.startDeployment(false)

	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())
}

func TestAutoscaler_ScaleUpOnlyDuringDeployment(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(map[string]interface{}{
		"scale_during_deployment": "up_only",
	}), nil)
	h.startDeployment(false)

	// Scaling up is allowed.
	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())

	// Scaling down is not.
	h.clock.Advance(2 * time.Minute)
	h.apm.value = 1
	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())
}

func TestAutoscaler_NoScalingWithUnpromotedCanary(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(map[string]interface{}{
		"scale_during_deployment": "up_only",
	}), nil)
	h.startDeployment(true)

	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())
}

// setQuota attaches a quota with an allocation limit to the default
// namespace.
func (h *harness) setQuota(allocs int) {
	h.index++
	must.NoError(h.t, h.store.UpsertQuotaSpecs(structs.MsgTypeTestSetup, h.index, []*structs.QuotaSpec{{
		Name: "q",
		Limits: []*structs.QuotaLimit{{
			Region:      "global",
			RegionLimit: &structs.Resources{},
			Allocations: pointer.Of(allocs),
		}},
	}}))
	h.index++
	must.NoError(h.t, h.store.UpsertNamespaces(h.index, []*structs.Namespace{{
		Name:  structs.DefaultNamespace,
		Quota: "q",
	}}))
}

func TestAutoscaler_ScaleUpCappedAtQuota(t *testing.T) {
	ci.Parallel(t)
	h, rec := newEventHarness(t)

	// 2 running allocations, room for one more.
	h.setQuota(3)
	rem, err := h.store.QuotaRemainingForNamespace(nil, structs.DefaultNamespace)
	must.NoError(t, err)
	must.NotNil(t, rem)

	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(3), *reqs[0].Count)
	must.StrContains(t, reqs[0].Message, `by quota "q"`)

	events := rec.list()
	must.Len(t, 2, events)
	must.Eq(t, structs.TypeScalingQuotaCapped, events[0].typ)
	must.Eq(t, int64(4), events[0].ev.DesiredCount)
	must.Eq(t, int64(3), events[0].ev.Count)
	must.Eq(t, "q", events[0].ev.Quota)
	must.Eq(t, structs.TypeScalingDecision, events[1].typ)
	must.Eq(t, int64(3), events[1].ev.Count)
}

func TestAutoscaler_QuotaExhausted(t *testing.T) {
	ci.Parallel(t)
	h, rec := newEventHarness(t)
	h.setQuota(2)

	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())
	events := rec.list()
	must.Len(t, 1, events)
	must.Eq(t, structs.TypeScalingQuotaCapped, events[0].typ)
}

func TestValidatePolicy(t *testing.T) {
	ci.Parallel(t)

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.APMs = []*PluginConfig{{Name: "prometheus", Driver: "prometheus"}}
	cfg.Strategies = []*PluginConfig{{Name: "pid", Driver: "pid-plugin"}}

	valid := func() *structs.ScalingPolicy {
		return &structs.ScalingPolicy{
			Type:               structs.ScalingPolicyTypeHorizontal,
			Engine:             structs.ScalingPolicyEngineBuiltin,
			Min:                1,
			Max:                10,
			EvaluationInterval: 30 * time.Second,
			Cooldown:           2 * time.Minute,
			OnCheckError:       "ignore",
			Checks: []*structs.ScalingCheck{
				{Name: "cpu", Metric: "nomad.cpu", Strategy: "target-value",
					StrategyConfig: map[string]string{"target": "70"}},
				{Name: "latency", Source: "prometheus", Query: "q", Strategy: "threshold",
					StrategyConfig: map[string]string{"upper": "0.3", "delta": "1"}},
				{Name: "fixed", Strategy: "fixed-value", StrategyConfig: map[string]string{"value": "3"}},
				{Name: "ext", Query: "q", Strategy: "pid"},
			},
		}
	}
	must.NoError(t, ValidatePolicy(valid(), cfg))

	cases := []struct {
		name string
		fn   func(p *structs.ScalingPolicy)
		err  string
	}{
		{"no checks", func(p *structs.ScalingPolicy) { p.Checks = nil }, "at least one check"},
		{"interval too low", func(p *structs.ScalingPolicy) { p.EvaluationInterval = time.Second },
			"evaluation_interval must be at least 10s"},
		{"cooldown too low", func(p *structs.ScalingPolicy) { p.CooldownOnScaleUp = time.Second },
			"cooldown_on_scale_up must be at least 10s"},
		{"on_check_error", func(p *structs.ScalingPolicy) { p.OnCheckError = "maybe" }, "on_check_error"},
		{"scale_during_deployment", func(p *structs.ScalingPolicy) { p.ScaleDuringDeployment = "always" },
			"scale_during_deployment"},
		{"unknown strategy", func(p *structs.ScalingPolicy) { p.Checks[0].Strategy = "magic" },
			`unknown strategy "magic"`},
		{"missing strategy", func(p *structs.ScalingPolicy) { p.Checks[0].Strategy = "" }, "missing strategy"},
		{"missing target", func(p *structs.ScalingPolicy) { p.Checks[0].StrategyConfig = nil },
			"missing required field `target`"},
		{"threshold bounds", func(p *structs.ScalingPolicy) { p.Checks[1].StrategyConfig = map[string]string{"delta": "1"} },
			"must have either"},
		{"fixed value", func(p *structs.ScalingPolicy) { p.Checks[2].StrategyConfig["value"] = "x" },
			"is not an integer"},
		{"unknown source", func(p *structs.ScalingPolicy) { p.Checks[1].Source = "datadog" },
			`unknown source "datadog"`},
		{"unknown metric", func(p *structs.ScalingPolicy) { p.Checks[0].Metric = "nomad.gpu" },
			`unknown metric "nomad.gpu"`},
		{"metric and query", func(p *structs.ScalingPolicy) { p.Checks[0].Query = "q" },
			"metric can't be combined"},
		{"no query", func(p *structs.ScalingPolicy) { p.Checks[1].Query = "" }, "requires a query or a metric"},
		{"duplicate check", func(p *structs.ScalingPolicy) { p.Checks[1].Name = "cpu" }, `duplicate check "cpu"`},
		{"instant threshold", func(p *structs.ScalingPolicy) { p.Checks[1].QueryInstant = true },
			"within_bounds_trigger"},
		{"check on_error", func(p *structs.ScalingPolicy) { p.Checks[1].OnError = "x" }, "on_error"},
		{"task policy", func(p *structs.ScalingPolicy) {
			p.Target = map[string]string{structs.ScalingTargetTask: "t"}
		}, "task scaling policies"},
		{"document error", func(p *structs.ScalingPolicy) {
			*p = structs.ScalingPolicy{Policy: map[string]interface{}{"cooldown": "soon"}}
			_ = p.FillTypedPolicy()
		}, "policy.cooldown"},
		{"bad schedule", func(p *structs.ScalingPolicy) {
			p.Policy = map[string]interface{}{"schedule": []interface{}{map[string]interface{}{"start": "nonsense"}}}
		}, "policy.schedule"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid()
			tc.fn(p)
			must.ErrorContains(t, ValidatePolicy(p, cfg), tc.err)
		})
	}

	// The lower bounds come from the configuration.
	p := valid()
	p.EvaluationInterval = 5 * time.Second
	cfg.MinEvalInterval = time.Second
	must.NoError(t, ValidatePolicy(p, cfg))
}
