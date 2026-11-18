// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/bacanoo-io/norca-rig/host"
	"github.com/bacanoo-io/norca-rig/internal/lang"
	"github.com/bacanoo-io/norca-rig/resource"
)

// Config is what an evaluation reads besides the module and its values.
type Config struct {
	// FS holds the files of the module, for file() and templatefile().
	FS fs.FS
	// FileBytes is the largest file the functions read.
	FileBytes int64
	// Resources is the most resources an evaluation may give.
	Resources int
	// Jobs decodes the job blocks; nil refuses a module with a job.
	Jobs host.JobDecoder
}

// Result is an evaluated module.
type Result struct {
	// Resources are in the order the server applies them (design 5), by
	// name within a kind.
	Resources []Resource
	// Outputs are by name.
	Outputs []Output
}

// Resource is an evaluated resource.
type Resource struct {
	Kind resource.Kind
	// Name is the name of the resource file; with for_each or count, the
	// key follows in brackets: workers["a"], replica[0].
	Name string
	// Spec is the resource in the encoding of the host: for a job the
	// decoder's, for the other kinds the value of its block as JSON.
	Spec json.RawMessage
	// Secrets are the items of a Variable that took a sensitive value
	// (design 6.3); they are not in Spec.
	Secrets map[string]host.Sensitive
}

// Output is an evaluated output.
type Output struct {
	Name        string
	Description string
	// Value is the value as JSON; nil when the output is sensitive.
	Value     json.RawMessage
	Sensitive bool
}

// Evaluate evaluates m with the variables vars (from Variables) and the
// facts of the install: the locals, the checks, the resources and the
// outputs (design 4.2).
func Evaluate(ctx context.Context, m *lang.Module, vars cty.Value, facts Facts, cfg Config) (*Result, hcl.Diagnostics) {
	base := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var":    vars,
			"rig":    facts.value(),
			"module": cty.EmptyObjectVal,
		},
		Functions: moduleFunctions(cfg.FS, cfg.FileBytes),
	}

	locals, diags := evalLocals(m.Locals, base)
	if diags.HasErrors() {
		return nil, diags
	}

	base.Variables[localsRoot] = locals

	if diags = append(diags, evalChecks(m.Checks, base)...); diags.HasErrors() {
		return nil, diags
	}

	e := &evaluator{ctx: ctx, cfg: cfg, sources: m.Sources, base: base}

	res := &Result{}

	for _, r := range m.Resources {
		if err := ctx.Err(); err != nil {
			return nil, append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Evaluation stopped",
				Detail: "The evaluation was stopped: " + err.Error() + ".", Subject: r.Block.DefRange.Ptr(),
			})
		}

		more, moreDiags := e.resource(r)
		diags = append(diags, moreDiags...)
		res.Resources = append(res.Resources, more...)

		if cfg.Resources > 0 && len(res.Resources) > cfg.Resources {
			return nil, append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Too many resources",
				Detail:  fmt.Sprintf("The module gives more than %d resources, the limit of the engine.", cfg.Resources),
				Subject: r.Block.DefRange.Ptr(),
			})
		}
	}

	outputs, moreDiags := evalOutputs(m.Outputs, base)
	diags = append(diags, moreDiags...)

	if diags.HasErrors() {
		return nil, diags
	}

	res.Outputs = outputs
	sortResources(res.Resources)

	return res, diags
}

// sortResources sorts resources in the order of their kinds (design 5),
// then by name.
func sortResources(rs []Resource) {
	order := resource.Builtin()
	rank := func(k resource.Kind) int {
		if i := slices.Index(order, k); i >= 0 {
			return i
		}

		return len(order)
	}

	slices.SortStableFunc(rs, func(a, b Resource) int {
		if d := rank(a.Kind) - rank(b.Kind); d != 0 {
			return d
		}

		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})
}

// evalChecks evaluates the check blocks: a false condition stops the
// evaluation with the error message of the check (design 5).
func evalChecks(blocks []*hcl.Block, ctx *hcl.EvalContext) hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, b := range blocks {
		var c struct {
			Condition    hcl.Expression `hcl:"condition"`
			ErrorMessage string         `hcl:"error_message"`
		}

		if moreDiags := gohcl.DecodeBody(b.Body, nil, &c); moreDiags.HasErrors() {
			diags = append(diags, moreDiags...)

			continue
		}

		v, moreDiags := c.Condition.Value(ctx)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			continue
		}

		v, _ = v.Unmark()

		switch {
		case v.IsNull() || !v.IsKnown() || v.Type() != cty.Bool:
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Invalid check",
				Detail:  fmt.Sprintf("The condition of the check %q must be true or false.", b.Labels[0]),
				Subject: c.Condition.Range().Ptr(),
			})
		case v.False():
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Check failed: " + b.Labels[0],
				Detail: c.ErrorMessage, Subject: c.Condition.Range().Ptr(),
			})
		}
	}

	return diags
}

// evalOutputs evaluates the output blocks. An output that takes a sensitive
// value says so, or it is an error.
func evalOutputs(blocks []*hcl.Block, ctx *hcl.EvalContext) ([]Output, hcl.Diagnostics) {
	var (
		outputs []Output
		diags   hcl.Diagnostics
	)

	for _, b := range blocks {
		var o struct {
			Value       hcl.Expression `hcl:"value"`
			Description string         `hcl:"description,optional"`
			Sensitive   bool           `hcl:"sensitive,optional"`
		}

		if moreDiags := gohcl.DecodeBody(b.Body, nil, &o); moreDiags.HasErrors() {
			diags = append(diags, moreDiags...)

			continue
		}

		v, moreDiags := o.Value.Value(ctx)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			continue
		}

		out := Output{Name: b.Labels[0], Description: o.Description, Sensitive: o.Sensitive}

		switch {
		case o.Sensitive:
		case v.ContainsMarked():
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Output takes a sensitive value",
				Detail:  fmt.Sprintf("The output %q takes a sensitive value; write sensitive = true on it.", out.Name),
				Subject: o.Value.Range().Ptr(),
			})

			continue
		default:
			data, err := marshal(v)
			if err != nil {
				diags = append(diags, valueDiag("output "+out.Name, err, o.Value.Range()))

				continue
			}

			out.Value = data
		}

		outputs = append(outputs, out)
	}

	slices.SortFunc(outputs, func(a, b Output) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})

	return outputs, diags
}

// marshal encodes an unmarked value as JSON.
func marshal(v cty.Value) (json.RawMessage, error) {
	if !v.IsWhollyKnown() {
		return nil, errors.New("the value is not known at install time")
	}

	return ctyjson.Marshal(v, v.Type())
}

func valueDiag(what string, err error, rng hcl.Range) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError, Summary: "Value not encoded",
		Detail:  fmt.Sprintf("The value of %s could not be encoded: %s.", what, err),
		Subject: rng.Ptr(),
	}
}

func sensitiveDiag(what string, rng hcl.Range) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError, Summary: "Sensitive value",
		Detail: fmt.Sprintf("A sensitive value can't be used in %s. A sensitive value goes only into an item of a "+
			"Variable, where it becomes a secret item of the release (design 6.3).", what),
		Subject: rng.Ptr(),
	}
}
