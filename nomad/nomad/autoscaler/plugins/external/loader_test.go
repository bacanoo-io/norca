// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package external

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/external/externaltest"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

type testClock struct {
	l   sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.l.Lock()
	defer c.l.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.l.Lock()
	defer c.l.Unlock()
	c.now = c.now.Add(d)
}

func newTestLoader(t *testing.T, dir string) (*Loader, *testClock) {
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	l := NewLoader(hclog.NewNullLogger(), dir)
	l.now = clock.Now
	t.Cleanup(l.Shutdown)
	return l, clock
}

func waitFor(f func() bool) *wait.Constraint {
	return wait.InitialSuccess(wait.BoolFunc(f), wait.Timeout(10*time.Second), wait.Gap(10*time.Millisecond))
}

func testRange() sdk.TimeRange {
	now := time.Now()
	return sdk.TimeRange{From: now.Add(-time.Minute), To: now}
}

func TestLoader_APMAndStrategy(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	externaltest.InstallPlugin(t, dir, "test-apm", "test-strategy")

	l, _ := newTestLoader(t, dir)

	apm, err := l.APM("my-apm", "test-apm", nil, map[string]string{"offset": "1"})
	must.NoError(t, err)
	info, err := apm.PluginInfo()
	must.NoError(t, err)
	must.Eq(t, "test-apm", info.Name)
	must.Eq(t, sdk.PluginTypeAPM, info.PluginType)

	ms, err := apm.Query("4", testRange())
	must.NoError(t, err)
	must.Len(t, 1, ms)
	must.Eq(t, 5.0, ms[0].Value)

	multi, err := apm.QueryMultiple("2", testRange())
	must.NoError(t, err)
	must.Len(t, 1, multi)
	must.Eq(t, 3.0, multi[0][0].Value)

	// Errors of the plugin are returned to the caller.
	_, err = apm.Query("not-a-number", testRange())
	must.ErrorContains(t, err, "invalid query")

	// A new configuration is applied to the running plugin.
	must.NoError(t, apm.SetConfig(map[string]string{"offset": "10"}))
	ms, err = apm.Query("4", testRange())
	must.NoError(t, err)
	must.Eq(t, 14.0, ms[0].Value)

	strategy, err := l.Strategy("my-strategy", "test-strategy", []string{"-type=strategy"}, nil)
	must.NoError(t, err)

	eval := &sdk.ScalingCheckEvaluation{
		Check: &sdk.ScalingPolicyCheck{
			Name:        "cpu",
			Source:      "my-apm",
			Query:       "7",
			QueryWindow: time.Minute,
			Strategy:    &sdk.ScalingPolicyStrategy{Name: "my-strategy", Config: map[string]string{"a": "b"}},
		},
		Metrics: sdk.TimestampedMetrics{{Timestamp: time.Now(), Value: 7}},
		Action:  &sdk.ScalingAction{Meta: map[string]interface{}{"nomad_policy_id": "p1"}},
	}
	out, err := strategy.Run(eval, 2)
	must.NoError(t, err)
	must.Eq(t, int64(7), out.Action.Count)
	must.Eq(t, sdk.ScaleDirection(sdk.ScaleDirectionUp), out.Action.Direction)
	must.Eq(t, "test-strategy: check cpu wants 7", out.Action.Reason)
	must.Eq[any](t, "p1", out.Action.Meta["nomad_policy_id"])
}

func TestLoader_CrashIsolationAndRestart(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	externaltest.InstallPlugin(t, dir, "test-apm")

	l, clock := newTestLoader(t, dir)
	l.RestartBackoff = time.Minute
	l.MaxRestartBackoff = 4 * time.Minute

	apm, err := l.APM("my-apm", "test-apm", nil, map[string]string{"offset": "1"})
	must.NoError(t, err)
	_, err = apm.Query("1", testRange())
	must.NoError(t, err)

	// The plugin process crashes: only the call fails.
	_, err = apm.Query("crash", testRange())
	must.Error(t, err)

	// Wait for go-plugin to notice the exit; afterwards calls fail fast
	// until the restart backoff has passed.
	inst := apm.(*apmInstance).instance
	must.Wait(t, waitFor(func() bool {
		inst.l.Lock()
		defer inst.l.Unlock()
		return inst.client != nil && inst.client.Exited()
	}))
	_, err = apm.Query("1", testRange())
	must.ErrorContains(t, err, "next start attempt")

	// After the backoff the plugin is restarted with its configuration.
	clock.Advance(time.Minute)
	ms, err := apm.Query("1", testRange())
	must.NoError(t, err)
	must.Eq(t, 2.0, ms[0].Value)

	// A second crash soon after doubles the backoff.
	_, err = apm.Query("crash", testRange())
	must.Error(t, err)
	must.Wait(t, waitFor(func() bool {
		inst.l.Lock()
		defer inst.l.Unlock()
		return inst.client != nil && inst.client.Exited()
	}))
	// The exit is noticed on the next call, which starts the backoff.
	_, err = apm.Query("1", testRange())
	must.ErrorContains(t, err, "next start attempt in 2m0s")
	clock.Advance(time.Minute)
	_, err = apm.Query("1", testRange())
	must.ErrorContains(t, err, "next start attempt")
	clock.Advance(time.Minute)
	_, err = apm.Query("1", testRange())
	must.NoError(t, err)
}

func TestLoader_MissingBinaryAndTypeMismatch(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()

	l, clock := newTestLoader(t, dir)
	l.RestartBackoff = time.Second

	// A missing binary does not fail the load: the plugin is started when
	// it is installed.
	apm, err := l.APM("my-apm", "test-apm", nil, nil)
	must.NoError(t, err)
	_, err = apm.Query("1", testRange())
	must.ErrorContains(t, err, "next start attempt")
	clock.Advance(time.Second)
	_, err = apm.Query("1", testRange())
	must.ErrorContains(t, err, "not found")

	externaltest.InstallPlugin(t, dir, "test-apm")
	clock.Advance(time.Minute)
	ms, err := apm.Query("3", testRange())
	must.NoError(t, err)
	must.Eq(t, 3.0, ms[0].Value)

	// A binary serving another plugin type is rejected.
	s, err := l.Strategy("s", "test-apm", nil, nil)
	must.NoError(t, err)
	_, err = s.Run(&sdk.ScalingCheckEvaluation{Action: &sdk.ScalingAction{}}, 1)
	must.Error(t, err)
}

func TestLoader_Shutdown(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	externaltest.InstallPlugin(t, dir, "test-apm")

	l, _ := newTestLoader(t, dir)
	apm, err := l.APM("my-apm", "test-apm", nil, nil)
	must.NoError(t, err)
	inst := apm.(*apmInstance).instance
	inst.l.Lock()
	client := inst.client
	inst.l.Unlock()
	must.NotNil(t, client)

	l.Shutdown()
	must.True(t, client.Exited())
	_, err = apm.Query("1", testRange())
	must.ErrorIs(t, err, ErrShutdown)

	_, err = l.APM("other", "test-apm", nil, nil)
	must.ErrorIs(t, err, ErrShutdown)
}

func TestDiscover(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()

	names, err := Discover(filepath.Join(dir, "missing"))
	must.NoError(t, err)
	must.Len(t, 0, names)

	must.NoError(t, os.WriteFile(filepath.Join(dir, externaltest.BinaryName("b-plugin")), []byte("x"), 0o755))
	must.NoError(t, os.WriteFile(filepath.Join(dir, externaltest.BinaryName("a-plugin")), []byte("x"), 0o755))
	must.NoError(t, os.WriteFile(filepath.Join(dir, "README.txt"), []byte("x"), 0o644))
	must.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755))

	names, err = Discover(dir)
	must.NoError(t, err)
	must.Eq(t, []string{"a-plugin", "b-plugin"}, names)

	_, err = resolve(dir, "../escape")
	must.ErrorContains(t, err, "invalid plugin driver")
	_, err = resolve(dir, "c-plugin")
	must.ErrorContains(t, err, "available: a-plugin, b-plugin")
	path, err := resolve(dir, "a-plugin")
	must.NoError(t, err)
	must.Eq(t, filepath.Join(dir, externaltest.BinaryName("a-plugin")), path)
}
