// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/resource"
	"github.com/bacanoo-io/norca/rigger/runref"
)

// Config is what an evaluation reads besides the module and its values.
type Config struct {
	// FileBytes is the largest file the functions read.
	FileBytes int64
	// Resources is the most resources an evaluation may give.
	Resources int
	// Jobs decodes the job blocks; nil refuses a module with a job.
	Jobs host.JobDecoder
	// Decoder decodes the resources of the kinds it names from their block
	// written as HCL; nil leaves every other kind the value of its block.
	Decoder host.ResourceDecoder
	// Test asks for the values a test reads: Resource.Value and
	// Result.OutputValues.
	Test bool
}

// Result is an evaluated module.
type Result struct {
	// Resources are in the order the server applies them (design 5), by
	// name within a kind.
	Resources []Resource
	// Outputs are by name.
	Outputs []Output
	// OutputValues is the object of the outputs: output.* of a test, and
	// module.<release> of a stack. A sensitive output is marked.
	OutputValues cty.Value
}

// Resource is an evaluated resource.
type Resource struct {
	Kind resource.Kind
	// Name is the name of the resource file; with for_each or count, the
	// key follows in brackets: workers["a"], replica[0]. A resource of a
	// child module has the name of the module block before it: db.backup.
	Name string
	// Spec is the resource in the encoding of the host: for a job the
	// decoder's, for the other kinds the value of its block as JSON.
	Spec json.RawMessage
	// Secrets are the items of a Variable that took a sensitive value
	// (design 6.3); they are not in Spec.
	Secrets map[string]host.Sensitive
	// Hook makes a job a hook of the release.
	Hook *resource.Hook

	// Block is the block type of the resource file: job, policy, variable.
	Block string
	// Base is the name without the key; Key is the key of for_each (a
	// string) or the index of count (a number), cty.NilVal for neither.
	Base string
	Key  cty.Value
	// Value is the block as a test reads it, with Config.Test: its
	// attributes and blocks as written (design 7.4), sensitive values
	// marked, runtime references as their functions give them.
	Value cty.Value
}

// Output is an evaluated output.
type Output struct {
	Name        string
	Description string
	// Value is the value as JSON; nil when the output is sensitive.
	Value     json.RawMessage
	Sensitive bool
}

// run is one evaluation, with the module that was installed and its
// children.
type run struct {
	ctx       context.Context //nolint:containedctx // lives for one call of Evaluate
	cfg       Config
	resources int
	expansion expansion
}

// Evaluate evaluates m with the variables vars (from Variables) and the
// facts of the installation: the locals and child modules, the checks, the
// resources and the outputs (design 4.2).
func Evaluate(ctx context.Context, m *lang.Module, vars cty.Value, facts Facts, cfg Config) (*Result, hcl.Diagnostics) {
	if m.Metadata.Library {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Library module",
			Detail: fmt.Sprintf("The module %q is a library: another module calls it, it is not installed on its own (design 6.5).",
				m.Metadata.Name),
		}}
	}

	r := &run{ctx: ctx, cfg: cfg}

	res, outputs, diags := r.module(m, vars, facts, "")
	if diags.HasErrors() {
		return nil, diags
	}

	res.OutputValues = outputs

	sortResources(res.Resources)

	return res, diags
}

// module evaluates a module; prefix goes before the names of its resources.
// It returns the object module.<name> of the module that calls it too.
func (r *run) module(m *lang.Module, vars cty.Value, facts Facts, prefix string) (*Result, cty.Value, hcl.Diagnostics) {
	base := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			varRoot:    vars,
			riggerRoot: facts.value(),
			moduleRoot: cty.EmptyObjectVal,
			localsRoot: cty.EmptyObjectVal,
		},
		Functions: moduleFunctions(m.FS, r.cfg.FileBytes),
	}

	res := &Result{}

	diags := r.values(m, base, facts, prefix, res)
	if diags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	if diags = append(diags, evalChecks(m.Checks, base)...); diags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	e := &evaluator{run: r, sources: m.Sources, base: base}

	for _, lr := range m.Resources {
		if over := stopped(r.ctx, lr.Block.DefRange); over.HasErrors() {
			return nil, cty.NilVal, append(diags, over...)
		}

		more, moreDiags := e.resource(lr)
		diags = append(diags, moreDiags...)

		for _, x := range more {
			x.Name, x.Base = prefix+x.Name, prefix+x.Base
			res.Resources = append(res.Resources, x)
		}

		if moreDiags := r.count(len(more), lr.Block.DefRange); moreDiags.HasErrors() {
			return nil, cty.NilVal, append(diags, moreDiags...)
		}
	}

	outputs, values, moreDiags := evalOutputs(m.Outputs, base)
	diags = append(diags, moreDiags...)

	if diags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	res.Outputs = outputs

	return res, values, diags
}

// stopped is the error of an evaluation whose time is up or that was
// cancelled, where it stops.
func stopped(ctx context.Context, rng hcl.Range) hcl.Diagnostics {
	err := ctx.Err()
	if err == nil {
		return nil
	}

	return hcl.Diagnostics{{
		Severity: hcl.DiagError, Summary: "Evaluation stopped",
		Detail: "The evaluation was stopped: " + err.Error() + ".", Subject: rng.Ptr(),
	}}
}

// count counts n more resources against the limit of the engine.
func (r *run) count(n int, rng hcl.Range) hcl.Diagnostics {
	diags := r.room(n, rng)
	r.resources += n

	return diags
}

// room reports n more resources over the limit, before they are made.
func (r *run) room(n int, rng hcl.Range) hcl.Diagnostics {
	if r.cfg.Resources == 0 || n <= r.cfg.Resources-r.resources {
		return nil
	}

	return hcl.Diagnostics{{
		Severity: hcl.DiagError, Summary: "Too many resources",
		Detail:  fmt.Sprintf("The module gives more than %d resources, the limit of the engine.", r.cfg.Resources),
		Subject: rng.Ptr(),
	}}
}

// values evaluates the locals and the child modules of m in the order of
// their references, into local.* and module.* of base. The resources of the
// children go into res.
func (r *run) values(m *lang.Module, base *hcl.EvalContext, facts Facts, prefix string, res *Result) hcl.Diagnostics {
	nodes, diags := valueNodes(m.Locals, m.Children)
	if diags.HasErrors() {
		return diags
	}

	order, moreDiags := orderNodes(nodes)
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return diags
	}

	locals, modules := map[string]cty.Value{}, map[string]cty.Value{}

	for _, key := range order {
		n := nodes[key]
		name := key[strings.IndexByte(key, '.')+1:]

		if over := stopped(r.ctx, n.rng); over.HasErrors() {
			return append(diags, over...)
		}

		if n.local != nil {
			v, moreDiags := n.local.Expr.Value(base)
			diags = append(diags, moreDiags...)
			locals[name] = v
			base.Variables[localsRoot] = cty.ObjectVal(locals)

			continue
		}

		v, moreDiags := r.child(n.child, base, facts, prefix, res)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			v = cty.DynamicVal
		}

		modules[name] = v
		base.Variables[moduleRoot] = cty.ObjectVal(modules)
	}

	return diags
}

// child evaluates a child module with the values its module block gives,
// and returns its outputs.
func (r *run) child(c *lang.Child, base *hcl.EvalContext, facts Facts, prefix string, res *Result) (cty.Value, hcl.Diagnostics) {
	inputs := map[string]Input{}

	var diags hcl.Diagnostics

	for name, attr := range c.Args {
		v, moreDiags := attr.Expr.Value(base)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			continue
		}

		if v.ContainsMarked() {
			if decl, ok := c.Module.Variable(name); !ok || !decl.Sensitive {
				diags = append(diags, sensitiveDiag("a variable of a child module that is not sensitive", attr.Expr.Range()))

				continue
			}

			v, _ = v.UnmarkDeep()
		}

		inputs[name] = Input{Value: v, Range: attr.Expr.Range().Ptr()}
	}

	if diags.HasErrors() {
		return cty.NilVal, diags
	}

	vars, diags := Variables(c.Module, inputs)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}

	md := c.Module.Metadata
	facts.ModuleName, facts.ModuleVersion = md.Name, md.Version

	out, outputs, moreDiags := r.module(c.Module, vars, facts, prefix+c.Name+".")
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return cty.NilVal, diags
	}

	res.Resources = append(res.Resources, out.Resources...)

	return outputs, diags
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

		return strings.Compare(a.Name, b.Name)
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

// evalOutputs evaluates the output blocks, and returns them as the object
// a calling module reads too, where a sensitive output keeps its mark. An
// output that takes a sensitive value says so, or it is an error.
func evalOutputs(blocks []*hcl.Block, ctx *hcl.EvalContext) ([]Output, cty.Value, hcl.Diagnostics) {
	var (
		outputs []Output
		diags   hcl.Diagnostics
	)

	values := map[string]cty.Value{}

	for _, b := range blocks {
		out, v, moreDiags := evalOutput(b, ctx)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			continue
		}

		outputs = append(outputs, out)
		values[out.Name] = v
	}

	slices.SortFunc(outputs, func(a, b Output) int { return strings.Compare(a.Name, b.Name) })

	return outputs, cty.ObjectVal(values), diags
}

func evalOutput(b *hcl.Block, ctx *hcl.EvalContext) (Output, cty.Value, hcl.Diagnostics) {
	var o struct {
		Value       hcl.Expression `hcl:"value"`
		Description string         `hcl:"description,optional"`
		Sensitive   bool           `hcl:"sensitive,optional"`
	}

	if diags := gohcl.DecodeBody(b.Body, nil, &o); diags.HasErrors() {
		return Output{}, cty.NilVal, diags
	}

	v, diags := o.Value.Value(ctx)
	if diags.HasErrors() {
		return Output{}, cty.NilVal, diags
	}

	out := Output{Name: b.Labels[0], Description: o.Description, Sensitive: o.Sensitive}

	switch {
	case o.Sensitive:
		return out, v.Mark(Sensitive), diags
	case v.ContainsMarked():
		return Output{}, cty.NilVal, append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Output takes a sensitive value",
			Detail:  fmt.Sprintf("The output %q takes a sensitive value; write sensitive = true on it.", out.Name),
			Subject: o.Value.Range().Ptr(),
		})
	}

	data, err := marshal(v)
	if err != nil {
		return Output{}, cty.NilVal, append(diags, valueDiag("output "+out.Name, err, o.Value.Range()))
	}

	out.Value = data

	return out, v, diags
}

// marshal encodes an unmarked value as JSON, with its runtime references
// resolved as outside a task template: ${...} (package runref).
func marshal(v cty.Value) (json.RawMessage, error) {
	if !v.IsWhollyKnown() {
		return nil, errors.New("the value is not known at install time")
	}

	v, err := resolveRuntime(v)
	if err != nil {
		return nil, err
	}

	return ctyjson.Marshal(v, v.Type())
}

// resolveRuntime resolves the runtime references in the strings of v as
// outside a task template.
func resolveRuntime(v cty.Value) (cty.Value, error) {
	return cty.Transform(v, func(_ cty.Path, v cty.Value) (cty.Value, error) {
		if v.IsNull() || !v.IsKnown() || v.Type() != cty.String || !runref.Contains(v.AsString()) {
			return v, nil
		}

		s, err := runref.Replace(v.AsString(), func(p runref.Part) (string, error) {
			return p.Resolve(runref.PlaceJob)
		})
		if err != nil {
			return cty.NilVal, err
		}

		return cty.StringVal(s), nil
	})
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
