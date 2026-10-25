// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command testplugin is a minimal external autoscaler plugin used by the
// tests of the external package. With -type=apm (default) it is an APM whose
// queries are numbers returned as the metric value; the query "crash" makes
// the process exit. With -type=strategy it is a strategy that sets the count
// to the latest metric value. The plugin name is the binary name, as the
// host expects the plugin name to match its driver.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/external"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
)

func main() {
	pluginType := flag.String("type", sdk.PluginTypeAPM, "plugin type")
	flag.Parse()

	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")

	external.Serve(func(hclog.Logger) interface{} {
		switch *pluginType {
		case sdk.PluginTypeStrategy:
			return &strategy{name: name}
		default:
			return &apm{name: name}
		}
	})
}

type apm struct {
	name   string
	offset float64
}

func (a *apm) PluginInfo() (*plugins.PluginInfo, error) {
	return &plugins.PluginInfo{Name: a.name, PluginType: sdk.PluginTypeAPM}, nil
}

func (a *apm) SetConfig(config map[string]string) error {
	if v, ok := config["offset"]; ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("invalid offset: %w", err)
		}
		a.offset = f
	}
	return nil
}

func (a *apm) Query(q string, r sdk.TimeRange) (sdk.TimestampedMetrics, error) {
	if q == "crash" {
		os.Exit(3)
	}
	v, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid query %q", q)
	}
	return sdk.TimestampedMetrics{{Timestamp: r.To, Value: v + a.offset}}, nil
}

func (a *apm) QueryMultiple(q string, r sdk.TimeRange) ([]sdk.TimestampedMetrics, error) {
	m, err := a.Query(q, r)
	if err != nil {
		return nil, err
	}
	return []sdk.TimestampedMetrics{m}, nil
}

type strategy struct {
	name string
}

func (s *strategy) PluginInfo() (*plugins.PluginInfo, error) {
	return &plugins.PluginInfo{Name: s.name, PluginType: sdk.PluginTypeStrategy}, nil
}

func (s *strategy) SetConfig(map[string]string) error { return nil }

func (s *strategy) Run(eval *sdk.ScalingCheckEvaluation, count int64) (*sdk.ScalingCheckEvaluation, error) {
	if len(eval.Metrics) == 0 {
		return nil, fmt.Errorf("no metrics")
	}
	target := int64(eval.Metrics[len(eval.Metrics)-1].Value)
	eval.Action.Count = target
	eval.Action.Reason = fmt.Sprintf("%s: check %s wants %d", s.name, eval.Check.Name, target)
	switch {
	case target > count:
		eval.Action.Direction = sdk.ScaleDirectionUp
	case target < count:
		eval.Action.Direction = sdk.ScaleDirectionDown
	default:
		eval.Action.Direction = sdk.ScaleDirectionNone
	}
	return eval, nil
}
