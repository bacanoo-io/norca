// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca-rig/host"
	"github.com/bacanoo-io/norca-rig/internal/eval"
	"github.com/bacanoo-io/norca-rig/oplog"
	"github.com/bacanoo-io/norca-rig/resource"
)

// Install is what an evaluation installs: the release, where, and the values
// of the variables (design 12.3). The facts are the rig.* of the module.
type Install struct {
	Release   string
	Namespace string
	Region    string
	Values    Values
}

// Result is an evaluated module: plain data that serializes, except the
// secret items, which never do.
type Result struct {
	// Resources are in the order the server applies them (design 5), by
	// name within a kind.
	Resources []Resource `json:"resources"`
	// Outputs are by name.
	Outputs []Output `json:"outputs"`
}

// Resource is an evaluated resource.
type Resource struct {
	resource.Resource

	// Secrets are the items of a Variable that took a sensitive value; they
	// are not in Spec, and become secret items of the release (design 6.3).
	Secrets map[string]host.Sensitive `json:"-"`
}

// SecretItems returns the names of the secret items, sorted.
func (r Resource) SecretItems() []string {
	return slices.Sorted(maps.Keys(r.Secrets))
}

// Output is an evaluated output.
type Output struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Value is the value as JSON; nil when the output is sensitive.
	Value     json.RawMessage `json:"value,omitempty"`
	Sensitive bool            `json:"sensitive,omitempty"`
}

// Evaluate evaluates m for an install: the values of its variables, its
// locals and checks, its resources (a job through the job decoder of the
// engine) and its outputs (design 4.2). It is a pure function of the module,
// the install and the decoder: the same input gives the same result. The
// limits of the engine bound the time and the number of resources.
func (e *Engine) Evaluate(ctx context.Context, m *Module, in Install) (*Result, hcl.Diagnostics) {
	md := m.mod.Metadata

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "module.evaluate",
		"module", md.Name, "version", md.Version, "release", in.Release, "namespace", in.Namespace)

	res, diags := e.evaluate(ctx, m, in)
	span.End(ctx, diagsErr(diags))

	return res, diags
}

func (e *Engine) evaluate(ctx context.Context, m *Module, in Install) (*Result, hcl.Diagnostics) {
	if diags := checkInstall(in); diags.HasErrors() {
		return nil, diags
	}

	if e.limits.Duration > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, e.limits.Duration)
		defer cancel()
	}

	vars, diags := e.variables(ctx, m, in.Values)
	if diags.HasErrors() {
		return nil, diags
	}

	md := m.mod.Metadata
	facts := eval.Facts{
		Release: in.Release, Namespace: in.Namespace, Region: in.Region,
		ModuleName: md.Name, ModuleVersion: md.Version,
	}

	out, moreDiags := eval.Evaluate(ctx, m.mod, vars, facts, eval.Config{
		FileBytes: e.limits.FileBytes, Resources: e.limits.Resources, Jobs: e.jobs, Decoder: e.decoder,
	})
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return nil, diags
	}

	res := &Result{
		Resources: make([]Resource, 0, len(out.Resources)),
		Outputs:   make([]Output, 0, len(out.Outputs)),
	}

	for _, r := range out.Resources {
		res.Resources = append(res.Resources, Resource{
			Kind: r.Kind, Name: r.Name, Spec: r.Spec, Hook: r.Hook, Secrets: r.Secrets,
		})
	}

	for _, o := range out.Outputs {
		res.Outputs = append(res.Outputs, Output(o))
	}

	return res, diags
}

// checkInstall checks that the facts of an install are given.
func checkInstall(in Install) hcl.Diagnostics {
	var diags hcl.Diagnostics

	facts := []struct{ name, value string }{
		{"release", in.Release}, {"namespace", in.Namespace}, {"region", in.Region},
	}

	for _, f := range facts {
		if name := f.name; f.value == "" {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Missing " + name,
				Detail: "An install names its " + name + ".",
			})
		}
	}

	return diags
}

// valuesObject is the object of the values given for a module.
func valuesObject(values Values) cty.Value {
	m := make(map[string]cty.Value, len(values))
	for name, v := range values {
		m[name] = v.Value
	}

	return cty.ObjectVal(m)
}
