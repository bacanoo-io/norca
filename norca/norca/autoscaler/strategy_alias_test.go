// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
)

func aliasCheck(source, query, strategy string, cfg map[string]interface{}) map[string]interface{} {
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	c := map[string]interface{}{
		"strategy": []interface{}{map[string]interface{}{strategy: []interface{}{cfg}}},
	}
	if source != "" {
		c["source"] = source
	}
	if query != "" {
		c["query"] = query
	}
	return c
}

func aliasDoc(checks map[string]map[string]interface{}) map[string]interface{} {
	cs := map[string]interface{}{}
	for name, c := range checks {
		cs[name] = []interface{}{c}
	}
	return map[string]interface{}{
		"evaluation_interval": "10s",
		"cooldown":            "1m",
		"check":               []interface{}{cs},
	}
}

func aliasRunner(h *harness, apms map[string]*fakeAPM) {
	logger := hclog.NewNullLogger()
	host, err := newPluginHost(logger, h.cfg, h.deps, nil)
	must.NoError(h.t, err)
	host.apms["fake"] = h.apm
	for n, a := range apms {
		host.apms[n] = a
	}
	h.runner = newRunner(logger, h.cfg, h.deps, h.clock.Now, host)
	h.runner.usage = newUsageCollector()
	h.sync()
}

func aliasStatus(h *harness) *Evaluation {
	st := h.runner.status.get(h.policyID())
	if st == nil {
		return nil
	}
	return st
}

func aliasSetRunning(h *harness, n int) {
	for i := 2; i < n; i++ {
		h.addAlloc(structs.AllocClientStatusRunning)
	}
}

func aliasCounts(h *harness) []any {
	var out []any
	for _, r := range h.scaler.requests() {
		if r.Count == nil {
			out = append(out, "nil(dry-run)")
		} else {
			out = append(out, *r.Count)
		}
	}
	return out
}

// A built-in strategy named in the agent configuration works as the built-in
// strategy it configures, as admission takes it (review finding ASP-10): an
// alias of fixed-value queried a source and was held "no data", one of
// pass-through laid its value on the running instances and scaled 10 to 4.
func TestStrategyAlias_FixedValue(t *testing.T) {
	ci.Parallel(t)

	cfgFn := func(c *Config) {
		c.Strategies = []*PluginConfig{{Name: "fix", Driver: plugins.InternalStrategyFixedValue}}
	}
	doc := aliasDoc(map[string]map[string]interface{}{
		"f": aliasCheck("", "", "fix", map[string]interface{}{"value": 5}),
	})
	cfg := DefaultConfig()
	cfgFn(cfg)
	must.NoError(t, ValidatePolicy(&structs.ScalingPolicy{Policy: doc, Min: 1, Max: 10}, cfg))

	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, doc, cfgFn)
	aliasRunner(h, nil)
	err := h.evaluate()
	st := aliasStatus(h)
	t.Logf("ASP-10 alias fix: err=%v requests=%v status=%+v", err, aliasCounts(h), st)
	must.NoError(t, err)
	must.Eq(t, []any{int64(5)}, aliasCounts(h), must.Sprintf("status %+v", st))

	// Control: the built-in name.
	doc2 := aliasDoc(map[string]map[string]interface{}{
		"f": aliasCheck("", "", "fixed-value", map[string]interface{}{"value": 5}),
	})
	h2 := newHarness(t, structs.ScalingPolicyEngineBuiltin, doc2, nil)
	aliasRunner(h2, nil)
	must.NoError(t, h2.evaluate())
	t.Logf("ASP-10 control fixed-value: requests=%v", aliasCounts(h2))
	must.Eq(t, []any{int64(5)}, aliasCounts(h2))
}

func TestStrategyAlias_PassThrough(t *testing.T) {
	ci.Parallel(t)

	cfgFn := func(c *Config) {
		c.Strategies = []*PluginConfig{{Name: "pt", Driver: plugins.InternalStrategyPassThrough}}
		c.SettleTimeout = time.Second
	}
	run := func(strategy string) (*harness, []any) {
		doc := aliasDoc(map[string]map[string]interface{}{
			"p": aliasCheck("fake", "n", strategy, nil),
		})
		h := newHarness(t, structs.ScalingPolicyEngineBuiltin, doc, cfgFn)
		aliasRunner(h, nil)
		// desired 10, running 5, the metric asks for 10.
		h.setCount(10)
		aliasSetRunning(h, 5)
		h.apm.value = 10
		must.NoError(t, h.evaluate())
		first := aliasStatus(h)
		t.Logf("ASP-10 %s first eval: outcome=%s hold=%s checks=%+v", strategy, first.Outcome, first.HoldReason, first.Checks)
		h.clock.Advance(2 * time.Second) // past the settle timeout
		must.NoError(t, h.evaluate())
		return h, aliasCounts(h)
	}
	_, alias := run("pt")
	t.Logf("ASP-10 alias pt: requests=%v", alias)
	_, builtin := run("pass-through")
	t.Logf("ASP-10 control pass-through: requests=%v", builtin)
	must.Eq(t, builtin, alias)
	must.Len(t, 0, alias)
}
