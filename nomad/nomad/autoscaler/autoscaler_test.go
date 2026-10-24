// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	l   sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.l.Lock()
	defer c.l.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.l.Lock()
	defer c.l.Unlock()
	c.now = c.now.Add(d)
}

// fakeAPM returns a fixed metric value.
type fakeAPM struct {
	clock *fakeClock
	value float64
	err   error
}

func (f *fakeAPM) Query(_ string, _ sdk.TimeRange) (sdk.TimestampedMetrics, error) {
	if f.err != nil {
		return nil, f.err
	}
	return sdk.TimestampedMetrics{{Timestamp: f.clock.Now(), Value: f.value}}, nil
}

func (f *fakeAPM) QueryMultiple(q string, r sdk.TimeRange) ([]sdk.TimestampedMetrics, error) {
	m, err := f.Query(q, r)
	return []sdk.TimestampedMetrics{m}, err
}

// fakeScaler emulates Job.Scale on the state store: it records a scaling
// event at the fake clock's time and updates the group count.
type fakeScaler struct {
	t     *testing.T
	l     sync.Mutex
	store *state.StateStore
	clock *fakeClock
	index uint64
	reqs  []*structs.JobScaleRequest
	err   error
}

func (f *fakeScaler) Scale(args *structs.JobScaleRequest) (*structs.JobRegisterResponse, error) {
	f.l.Lock()
	defer f.l.Unlock()

	f.reqs = append(f.reqs, args)
	if f.err != nil {
		return nil, f.err
	}

	ns, group := args.Target[structs.ScalingTargetNamespace], args.Target[structs.ScalingTargetGroup]
	job, err := f.store.JobByID(nil, ns, args.JobID)
	must.NoError(f.t, err)
	job = job.Copy()
	tg := job.LookupTaskGroup(group)
	prev := int64(tg.Count)

	f.index++
	if args.Count != nil {
		tg.Count = int(*args.Count)
		must.NoError(f.t, f.store.UpsertJob(structs.MsgTypeTestSetup, f.index, nil, job))
		f.index++
	}
	must.NoError(f.t, f.store.UpsertScalingEvent(f.index, &structs.ScalingEventRequest{
		Namespace: ns,
		JobID:     args.JobID,
		TaskGroup: group,
		ScalingEvent: &structs.ScalingEvent{
			Time:          f.clock.Now().UnixNano(),
			PreviousCount: prev,
			Count:         args.Count,
			Message:       args.Message,
			Error:         args.Error,
			Meta:          args.Meta,
		},
	}))
	return &structs.JobRegisterResponse{}, nil
}

func (f *fakeScaler) requests() []*structs.JobScaleRequest {
	f.l.Lock()
	defer f.l.Unlock()
	return append([]*structs.JobScaleRequest(nil), f.reqs...)
}

type harness struct {
	t      *testing.T
	store  *state.StateStore
	clock  *fakeClock
	apm    *fakeAPM
	scaler *fakeScaler
	cfg    *Config
	deps   *Deps
	runner *runner
	job    *structs.Job
	index  uint64
}

// targetValuePolicy returns a policy document with one target-value check
// on the fake APM.
func targetValuePolicy(extra map[string]interface{}) map[string]interface{} {
	doc := map[string]interface{}{
		"evaluation_interval": "10s",
		"cooldown":            "1m",
		"check": []interface{}{
			map[string]interface{}{
				"cpu": []interface{}{
					map[string]interface{}{
						"source": "fake",
						"query":  "cpu",
						"strategy": []interface{}{
							map[string]interface{}{
								"target-value": []interface{}{
									map[string]interface{}{"target": 50},
								},
							},
						},
					},
				},
			},
		},
	}
	for k, v := range extra {
		doc[k] = v
	}
	return doc
}

func newHarness(t *testing.T, engine string, doc map[string]interface{}, cfgFn func(*Config)) *harness {
	store := state.TestStateStore(t)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}

	job, policy := mock.JobWithScalingPolicy()
	job.TaskGroups[0].Count = 2
	policy.Min = 1
	policy.Max = 10
	policy.Engine = engine
	policy.Policy = doc
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		a := mock.Alloc()
		a.Job = job
		a.JobID = job.ID
		a.Namespace = job.Namespace
		a.TaskGroup = job.TaskGroups[0].Name
		a.ClientStatus = structs.AllocClientStatusRunning
		allocs = append(allocs, a)
	}
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1001, allocs))

	h := &harness{
		t:      t,
		store:  store,
		clock:  clock,
		apm:    &fakeAPM{clock: clock, value: 100},
		scaler: &fakeScaler{t: t, store: store, clock: clock, index: 2000},
		cfg:    DefaultConfig(),
		job:    job,
		index:  3000,
	}
	h.cfg.Enabled = true
	if cfgFn != nil {
		cfgFn(h.cfg)
	}
	h.deps = &Deps{
		State:  func() *state.StateStore { return store },
		Scaler: h.scaler,
		Now:    clock.Now,
	}
	h.newRunner()
	return h
}

// newRunner creates a fresh runner, as a new leader would.
func (h *harness) newRunner() {
	logger := hclog.NewNullLogger()
	host, err := newPluginHost(logger, h.cfg, h.deps, nil)
	must.NoError(h.t, err)
	host.apms["fake"] = h.apm
	h.runner = newRunner(logger, h.cfg, h.deps, h.clock.Now, host)
	h.sync()
}

// sync makes the runner pick up the current policies.
func (h *harness) sync() {
	list, err := h.runner.builtinPolicies(nil, h.store)
	must.NoError(h.t, err)
	h.runner.reconcile(list, h.clock.Now())
}

func (h *harness) policyID() string {
	return h.job.TaskGroups[0].Scaling.ID
}

func (h *harness) evaluate() error {
	handle, ok := h.runner.handles[h.policyID()]
	must.True(h.t, ok)
	must.NotNil(h.t, handle.compiled)
	return h.runner.evaluate(context.Background(), handle.compiled)
}

// addEvent records a scaling event, as an earlier leader would have.
func (h *harness) addEvent(at time.Time, prev int64, count *int64, meta map[string]interface{}, isErr bool) {
	h.index++
	must.NoError(h.t, h.store.UpsertScalingEvent(h.index, &structs.ScalingEventRequest{
		Namespace: h.job.Namespace,
		JobID:     h.job.ID,
		TaskGroup: h.job.TaskGroups[0].Name,
		ScalingEvent: &structs.ScalingEvent{
			Time:          at.UnixNano(),
			PreviousCount: prev,
			Count:         count,
			Error:         isErr,
			Meta:          meta,
		},
	}))
}

func builtinMeta() map[string]interface{} {
	return map[string]interface{}{metaKeyEngine: structs.ScalingPolicyEngineBuiltin}
}

func TestAutoscaler_ScaleUp(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)

	must.NoError(t, h.evaluate())

	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	req := reqs[0]
	must.Eq(t, h.job.ID, req.JobID)
	must.Eq(t, h.job.Namespace, req.RequestNamespace())
	must.Eq(t, "web", req.Target[structs.ScalingTargetGroup])
	// 2 running allocations at 100% of a 50% target.
	must.Eq(t, int64(4), *req.Count)
	must.Eq[any](t, structs.ScalingPolicyEngineBuiltin, req.Meta[metaKeyEngine])
	must.Eq[any](t, h.policyID(), req.Meta["nomad_policy_id"])
}

func TestAutoscaler_ScaleCappedToMax(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.apm.value = 1000

	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(10), *reqs[0].Count)
}

func TestAutoscaler_NoActionWithinTarget(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.apm.value = 50

	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())
}

func TestAutoscaler_Cooldown(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)

	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())

	// Within the 1m cooldown no evaluation takes place.
	h.clock.Advance(30 * time.Second)
	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())

	// After the cooldown the policy scales again.
	h.clock.Advance(31 * time.Second)
	must.NoError(t, h.evaluate())
	must.Len(t, 2, h.scaler.requests())
}

func TestAutoscaler_CooldownRestoredAfterLeaderChange(t *testing.T) {
	ci.Parallel(t)
	doc := targetValuePolicy(map[string]interface{}{
		"cooldown":             "2m",
		"cooldown_on_scale_up": "1m",
	})
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, doc, nil)

	// The previous leader scaled the group down 90 seconds ago: the scale
	// down cooldown (2m) applies, not the scale up one.
	h.addEvent(h.clock.Now().Add(-90*time.Second), 3, pointer.Of(int64(2)), builtinMeta(), false)

	// A new leader starts with an empty memory.
	h.newRunner()
	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())

	h.clock.Advance(31 * time.Second)
	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())
}

func TestAutoscaler_ManualScalingStartsCooldown(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)

	// An operator scaled the job 10 seconds ago (no engine meta).
	h.addEvent(h.clock.Now().Add(-10*time.Second), 1, pointer.Of(int64(2)), nil, false)
	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())

	// Error events (no count change) do not start a cooldown.
	h2 := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h2.addEvent(h2.clock.Now().Add(-10*time.Second), 2, nil, nil, true)
	must.NoError(t, h2.evaluate())
	must.Len(t, 1, h2.scaler.requests())
}

func TestAutoscaler_OscillationProtection(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	now := h.clock.Now()

	// The group went up, down and up again within the window.
	h.addEvent(now.Add(-9*time.Minute), 2, pointer.Of(int64(3)), builtinMeta(), false)
	h.addEvent(now.Add(-8*time.Minute), 3, pointer.Of(int64(2)), builtinMeta(), false)
	h.addEvent(now.Add(-7*time.Minute), 2, pointer.Of(int64(3)), builtinMeta(), false)

	// Going down now would be the third direction change: the group is
	// suspended instead, and a warning event is written.
	h.apm.value = 10
	must.NoError(t, h.evaluate())

	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.Nil(t, reqs[0].Count)
	must.True(t, reqs[0].Error)
	until, err := strconv.ParseInt(reqs[0].Meta[metaKeySuspendedUntil].(string), 10, 64)
	must.NoError(t, err)
	must.Eq(t, now.Add(DefaultOscillationSuspend).UnixNano(), until)

	// The suspension is restored by a new leader from the warning event.
	h.newRunner()
	h.clock.Advance(5 * time.Minute)
	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())

	// After the suspension the old direction changes are out of the window
	// and the group scales again.
	h.clock.Advance(11 * time.Minute)
	must.NoError(t, h.evaluate())
	reqs = h.scaler.requests()
	must.Len(t, 2, reqs)
	must.Eq(t, int64(1), *reqs[1].Count)
}

func TestAutoscaler_OscillationIgnoresOtherScaling(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	now := h.clock.Now()

	// Direction changes made by others do not count.
	h.addEvent(now.Add(-9*time.Minute), 2, pointer.Of(int64(3)), nil, false)
	h.addEvent(now.Add(-8*time.Minute), 3, pointer.Of(int64(2)), nil, false)
	h.addEvent(now.Add(-7*time.Minute), 2, pointer.Of(int64(3)), nil, false)

	h.apm.value = 10
	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.False(t, reqs[0].Error)
	must.Eq(t, int64(1), *reqs[0].Count)
}

func TestAutoscaler_RateLimit(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), func(c *Config) {
		c.RateLimit = 1
		c.RateLimitBurst = 1
	})
	c := h.runner.handles[h.policyID()].compiled

	readT := func() *targetState {
		ts, err := readTarget(h.store, c.key)
		must.NoError(t, err)
		return ts
	}
	action := sdk.ScalingAction{Count: 4, Direction: sdk.ScaleDirectionUp}

	now := h.clock.Now()
	must.NoError(t, h.runner.exec.execute(c, readT(), action, now))
	must.NoError(t, h.runner.exec.execute(c, readT(), action, now))
	must.Len(t, 1, h.scaler.requests())

	// One token per second.
	must.NoError(t, h.runner.exec.execute(c, readT(), action, now.Add(time.Second)))
	must.Len(t, 2, h.scaler.requests())
}

func TestAutoscaler_ActiveDeploymentIsNoop(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.scaler.err = errors.New("rpc error: " + errActiveDeployment)

	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())

	// No cooldown after a no-op.
	h.scaler.err = nil
	must.NoError(t, h.evaluate())
	must.Len(t, 2, h.scaler.requests())
}

func TestAutoscaler_ScaleError(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.scaler.err = errors.New("permission denied")

	must.ErrorContains(t, h.evaluate(), "permission denied")
}

func TestAutoscaler_DryRun(t *testing.T) {
	ci.Parallel(t)
	doc := targetValuePolicy(map[string]interface{}{
		"target": []interface{}{
			map[string]interface{}{
				"nomad-target": []interface{}{
					map[string]interface{}{"dry-run": "true"},
				},
			},
		},
	})
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, doc, nil)

	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.Nil(t, reqs[0].Count)
	must.Eq[any](t, true, reqs[0].Meta["nomad_autoscaler.dry_run"])

	// Dry-run actions also start the cooldown.
	h.clock.Advance(10 * time.Second)
	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())
}

func TestAutoscaler_QueryErrorOnError(t *testing.T) {
	ci.Parallel(t)

	// By default a failing check is ignored.
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.apm.err = errors.New("apm down")
	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())

	// With on_check_error = "fail" the evaluation fails.
	h = newHarness(t, structs.ScalingPolicyEngineBuiltin,
		targetValuePolicy(map[string]interface{}{"on_check_error": "fail"}), nil)
	h.apm.err = errors.New("apm down")
	must.ErrorContains(t, h.evaluate(), "apm down")
}

func TestAutoscaler_EngineSelection(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name          string
		engine        string
		defaultEngine string
		tracked       bool
	}{
		{"builtin policy", "builtin", "external", true},
		{"external policy", "external", "builtin", false},
		{"default external", "", "external", false},
		{"default builtin", "", "builtin", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.engine, targetValuePolicy(nil), func(c *Config) {
				c.DefaultEngine = tc.defaultEngine
			})
			_, ok := h.runner.handles[h.policyID()]
			must.Eq(t, tc.tracked, ok)
		})
	}
}

func TestAutoscaler_PolicyUpdates(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	must.MapLen(t, 1, h.runner.handles)
	c := h.runner.handles[h.policyID()].compiled

	// Switching the job to the external engine stops tracking the policy,
	// and an evaluation already in progress does not scale.
	job := h.job.Copy()
	job.TaskGroups[0].Scaling.Engine = structs.ScalingPolicyEngineExternal
	must.NoError(t, h.store.UpsertJob(structs.MsgTypeTestSetup, 1100, nil, job))

	must.NoError(t, h.runner.evaluate(context.Background(), c))
	must.Len(t, 0, h.scaler.requests())

	h.sync()
	must.MapLen(t, 0, h.runner.handles)

	// An invalid policy is tracked but never evaluated.
	job = job.Copy()
	job.TaskGroups[0].Scaling.Engine = structs.ScalingPolicyEngineBuiltin
	job.TaskGroups[0].Scaling.Policy = map[string]interface{}{
		"check": []interface{}{
			map[string]interface{}{
				"cpu": []interface{}{
					map[string]interface{}{
						"source": "fake",
						"query":  "cpu",
						"strategy": []interface{}{
							map[string]interface{}{"no-such-strategy": []interface{}{map[string]interface{}{}}},
						},
					},
				},
			},
		},
	}
	must.NoError(t, h.store.UpsertJob(structs.MsgTypeTestSetup, 1200, nil, job))
	h.sync()
	must.MapLen(t, 1, h.runner.handles)
	must.Nil(t, h.runner.handles[h.policyID()].compiled)
}

func TestAutoscaler_ParsePolicy(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(map[string]interface{}{
		"evaluation_interval": "1s",
		"on_check_error":      "fail",
	}), func(c *Config) { c.MinEvalInterval = 5 * time.Second })

	p := h.runner.handles[h.policyID()].compiled.policy
	must.Eq(t, h.policyID(), p.ID)
	must.Eq(t, int64(1), p.Min)
	must.Eq(t, int64(10), p.Max)
	must.Eq(t, time.Minute, p.Cooldown)
	must.Eq(t, time.Minute, p.CooldownOnScaleUp)
	// The server's lower bound applies.
	must.Eq(t, 5*time.Second, p.EvaluationInterval)
	must.Eq(t, "fail", p.OnCheckError)
	must.Eq(t, "nomad-target", p.Target.Name)
	must.Len(t, 1, p.Checks)
	must.Eq(t, "cpu", p.Checks[0].Name)
	must.Eq(t, "fake", p.Checks[0].Source)
	must.Eq(t, "target-value", p.Checks[0].Strategy.Name)
	must.Eq(t, "50", p.Checks[0].Strategy.Config["target"])
	must.Eq(t, DefaultQueryWindow, p.Checks[0].QueryWindow)
}

func TestAutoscaler_NomadAPMQueryCanonicalized(t *testing.T) {
	ci.Parallel(t)
	doc := targetValuePolicy(nil)
	check := doc["check"].([]interface{})[0].(map[string]interface{})["cpu"].([]interface{})[0].(map[string]interface{})
	delete(check, "source")
	check["query"] = "avg_cpu"
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, doc, nil)

	c := h.runner.handles[h.policyID()].compiled.policy.Checks[0]
	must.Eq(t, "nomad-apm", c.Source)
	must.Eq(t, "taskgroup_avg_cpu/web/"+h.job.ID+"@default", c.Query)
}

func TestAutoscaler_Lifecycle(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	ready := make(chan struct{})
	a := New(hclog.NewNullLogger(), DefaultConfig(), &Deps{
		State:  func() *state.StateStore { return store },
		Scaler: &fakeScaler{t: t, store: store, clock: &fakeClock{now: time.Now()}},
		ServersReady: func() bool {
			select {
			case <-ready:
				return true
			default:
				return false
			}
		},
	})

	// Disabled by default.
	a.SetLeader(true)
	must.False(t, a.Running())
	must.Eq(t, structs.ScalingPolicyEngineExternal, a.DefaultEngine())

	// Enabled on reload.
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.DefaultEngine = structs.ScalingPolicyEngineBuiltin
	a.SetConfig(cfg)
	must.True(t, a.Running())
	must.Eq(t, structs.ScalingPolicyEngineBuiltin, a.DefaultEngine())

	// Stopped when losing leadership.
	a.SetLeader(false)
	must.False(t, a.Running())

	a.SetLeader(true)
	must.True(t, a.Running())
	close(ready)
	a.Stop()
	must.False(t, a.Running())
}

func TestAutoscaler_RunLoop(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(map[string]interface{}{
		"evaluation_interval": "20ms",
	}), func(c *Config) {
		c.MinEvalInterval = time.Millisecond
		c.EvalWorkers = 1
	})

	// Run the scheduling loop and the worker pool with the real clock.
	logger := hclog.NewNullLogger()
	host, err := newPluginHost(logger, h.cfg, h.deps, nil)
	must.NoError(t, err)
	host.apms["fake"] = h.apm
	r := newRunner(logger, h.cfg, h.deps, time.Now, host)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for len(h.scaler.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	reqs := h.scaler.requests()
	must.SliceNotEmpty(t, reqs)
	must.Eq(t, int64(4), *reqs[0].Count)
}
