// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/external"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/external/externaltest"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// externalPolicy returns a policy document with one check that uses the
// external test APM and strategy. The test APM returns the query as metric
// value and the test strategy proposes that value as count.
func externalPolicy(query string) map[string]interface{} {
	return map[string]interface{}{
		"cooldown": "1m",
		"check": []interface{}{
			map[string]interface{}{
				"external": []interface{}{
					map[string]interface{}{
						"source": "ext-apm",
						"query":  query,
						"strategy": []interface{}{
							map[string]interface{}{
								"ext-strategy": []interface{}{map[string]interface{}{}},
							},
						},
					},
				},
			},
		},
	}
}

func externalConfig(dir string) func(*Config) {
	return func(c *Config) {
		c.PluginDir = dir
		c.APMs = []*PluginConfig{{Name: "ext-apm", Driver: "test-apm", Config: map[string]string{"offset": "1"}}}
		c.Strategies = []*PluginConfig{{Name: "ext-strategy", Driver: "test-strategy", Args: []string{"-type=strategy"}}}
	}
}

// setPolicy updates the policy document of the harness job.
func (h *harness) setPolicy(doc map[string]interface{}) {
	job := h.job.Copy()
	job.TaskGroups[0].Scaling.Policy = doc
	h.index++
	must.NoError(h.t, h.store.UpsertJob(structs.MsgTypeTestSetup, h.index, nil, job))
	h.job = job
}

func TestAutoscaler_ExternalPlugins(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	externaltest.InstallPlugin(t, dir, "test-apm", "test-strategy")

	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, externalPolicy("6"), nil)
	externalConfig(dir)(h.cfg)

	loader := external.NewLoader(hclog.NewNullLogger(), dir)
	defer loader.Shutdown()
	host, err := newPluginHost(hclog.NewNullLogger(), h.cfg, h.deps, loader)
	must.NoError(t, err)
	h.runner = newRunner(hclog.NewNullLogger(), h.cfg, h.deps, h.clock.Now, host)
	h.sync()

	// The check runs through both plugin processes: the APM returns 6+1 and
	// the strategy proposes it as count.
	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(7), *reqs[0].Count)
	must.Eq(t, "test-strategy: check external wants 7", reqs[0].Message)
	must.Eq[any](t, h.policyID(), reqs[0].Meta["nomad_policy_id"])

	// A crash of the APM process only fails the evaluation of its check.
	h.clock.Advance(2 * time.Minute)
	h.setPolicy(externalPolicy("crash"))
	h.sync()
	must.NoError(t, h.evaluate())
	must.Len(t, 1, h.scaler.requests())

	// The restarted plugin serves the next evaluations.
	h.clock.Advance(2 * time.Minute)
	h.setPolicy(externalPolicy("2"))
	h.sync()
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		loader.RestartBackoff = 0
		if err := h.evaluate(); err != nil {
			return err
		}
		if n := len(h.scaler.requests()); n != 2 {
			return fmt.Errorf("%d scaling requests", n)
		}
		return nil
	}), wait.Timeout(20*time.Second), wait.Gap(50*time.Millisecond)))
	must.Eq(t, int64(3), *h.scaler.requests()[1].Count)
}

func TestAutoscaler_ExternalPluginsStopWithLeadership(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	externaltest.InstallPlugin(t, dir, "test-apm", "test-strategy")

	var (
		l       sync.Mutex
		loaders []*external.Loader
	)
	store := state.TestStateStore(t)
	cfg := DefaultConfig()
	cfg.Enabled = true
	externalConfig(dir)(cfg)
	a := New(hclog.NewNullLogger(), cfg, &Deps{
		State:  func() *state.StateStore { return store },
		Scaler: &fakeScaler{t: t, store: store, clock: &fakeClock{now: time.Now()}},
		ExternalPlugins: func(logger hclog.Logger, dir string) ExternalPluginLoader {
			loader := external.NewLoader(logger, dir)
			l.Lock()
			loaders = append(loaders, loader)
			l.Unlock()
			return loader
		},
	})

	a.SetLeader(true)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		l.Lock()
		defer l.Unlock()
		return len(loaders) == 1
	}), wait.Timeout(10*time.Second), wait.Gap(10*time.Millisecond)))

	// Revoking leadership stops the plugin processes.
	a.SetLeader(false)
	l.Lock()
	loader := loaders[0]
	l.Unlock()
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		_, err := loader.APM("late", "test-apm", nil, nil)
		return errors.Is(err, external.ErrShutdown)
	}), wait.Timeout(10*time.Second), wait.Gap(10*time.Millisecond)))
	a.Stop()
}
