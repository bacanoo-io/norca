// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// generatePrefix is the value form of a generated value (design 6.4, 11):
// "!generate:<name>" stands for a random value the stack makes once and
// keeps in the secret store of the cluster.
const generatePrefix = "!generate:"

// Generated gives the generated values of a stack by name; ok is false for a
// value that is not generated yet.
type Generated func(name string) (value string, ok bool)

// StackResult is an evaluated stack.
type StackResult struct {
	// Releases are in the order of their dependencies: a release comes
	// after the releases it reads or depends on.
	Releases []*ReleaseResult
	// Secrets are the secrets the stack creates once.
	Secrets []StackSecret
	// Generated are the names of the generated values the stack uses;
	// Missing those not generated yet.
	Generated []string
	Missing   []string
	Outputs   []Output
}

// ReleaseResult is an evaluated release of a stack.
type ReleaseResult struct {
	Release *lang.StackRelease
	// Name is the name of the release: that of the module block, after
	// <part>- for a release of a part (design 7.5).
	Name string
	// Enabled is false for a release written with enabled = false: it is
	// not installed (design 6.4).
	Enabled   bool
	Namespace string
	// Values are the values the stack gives the module, sensitive ones
	// marked.
	Values cty.Value
	Result *Result
	// DependsOn are the releases it reads or names in depends_on.
	DependsOn []string
	// Pending is set when a value is generated only at apply: the release
	// is evaluated again then.
	Pending bool
}

// StackSecret is a secret of secrets.hcl: created once, its items replaced
// only with overwrite.
type StackSecret struct {
	Path      string
	Items     map[string]string
	Overwrite bool
	Pending   bool
}

// stackRun evaluates a stack.
type stackRun struct {
	ctx       context.Context //nolint:containedctx // lives for one call of EvaluateStack
	stack     *lang.Stack
	cfg       Config
	generated Generated
	base      *hcl.EvalContext
	res       *StackResult
	used      map[string]bool
	// prefix goes before the names of the releases: <part>- for a part.
	prefix string
	// partReleases are the names of the releases of each part evaluated.
	partReleases map[string][]string
}

// EvaluateStack evaluates a stack with the values of its variables (from
// Variables over Stack.Vars): its locals and releases in the order of their
// references, each release a module evaluated with the values the stack
// gives it, then its checks, secrets and outputs.
func EvaluateStack(ctx context.Context, s *lang.Stack, vars cty.Value, generated Generated, cfg Config) (*StackResult, hcl.Diagnostics) {
	res, _, diags := newStackRun(ctx, s, vars, generated, cfg, "").run()

	return res, diags
}

// StackReleaseOrder returns the names of the releases of a stack in the
// order an apply installs them, each after those it reads or names in
// depends_on, the releases of a part (<part>-<name>) where the part is. It
// evaluates nothing: the order comes from the references of the blocks, as
// EvaluateStack takes it. A part whose stack was not fetched contributes
// nothing.
func StackReleaseOrder(s *lang.Stack) ([]string, hcl.Diagnostics) {
	return stackReleaseOrder(s, "")
}

func stackReleaseOrder(s *lang.Stack, prefix string) ([]string, hcl.Diagnostics) {
	r := &stackRun{stack: s, prefix: prefix}

	nodes, diags := r.nodes()
	if diags.HasErrors() {
		return nil, diags
	}

	order, moreDiags := orderNodes(nodes)
	if diags = append(diags, moreDiags...); moreDiags.HasErrors() {
		return nil, diags
	}

	var out []string

	for _, key := range order {
		n := nodes[key]

		switch {
		case n.release != nil:
			out = append(out, prefix+n.release.Name)
		case n.part != nil && n.part.Stack != nil:
			inner, moreDiags := stackReleaseOrder(n.part.Stack, prefix+n.part.Name+"-")
			if diags = append(diags, moreDiags...); moreDiags.HasErrors() {
				return nil, diags
			}

			out = append(out, inner...)
		}
	}

	return out, diags
}

func newStackRun(ctx context.Context, s *lang.Stack, vars cty.Value, generated Generated, cfg Config, prefix string) *stackRun {
	return &stackRun{
		ctx: ctx, stack: s, cfg: cfg, generated: generated, res: &StackResult{}, used: map[string]bool{},
		prefix: prefix, partReleases: map[string][]string{},
		base: &hcl.EvalContext{
			Variables: map[string]cty.Value{
				varRoot:    vars,
				riggerRoot: stackFacts(s),
				moduleRoot: cty.EmptyObjectVal,
				localsRoot: cty.EmptyObjectVal,
				stackRoot:  cty.EmptyObjectVal,
			},
			Functions: moduleFunctions(s.FS, cfg.FileBytes),
		},
	}
}

// run evaluates the stack; it returns its outputs as an object too.
func (r *stackRun) run() (*StackResult, cty.Value, hcl.Diagnostics) {
	s := r.stack

	diags := r.values()
	if diags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	if diags = append(diags, evalChecks(s.Checks, r.base)...); diags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	diags = append(diags, r.secrets()...)

	outputs, outputValues, moreDiags := evalOutputs(s.Outputs, r.base)
	diags = append(diags, moreDiags...)

	if diags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	r.res.Outputs = outputs

	for name := range r.used {
		r.res.Generated = append(r.res.Generated, name)
		if _, ok := r.generated(name); !ok {
			r.res.Missing = append(r.res.Missing, name)
		}
	}

	slices.Sort(r.res.Generated)
	slices.Sort(r.res.Missing)

	return r.res, outputValues, diags
}

// maxReleaseName is the longest name of a release.
const maxReleaseName = 63

// stackFacts is the object rigger.* of a stack.
func stackFacts(s *lang.Stack) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"stack":     cty.StringVal(s.Rigger.Name),
		"region":    cty.StringVal(StackRegion(s)),
		"namespace": cty.StringVal(StackNamespace(s)),
	})
}

// StackRegion is the region of a stack: global when it names none.
func StackRegion(s *lang.Stack) string {
	return or(s.Rigger.Region, "global")
}

// StackNamespace is the namespace of a release that names none.
func StackNamespace(s *lang.Stack) string {
	return or(s.Rigger.Namespace, "default")
}

// nodes gathers the locals, releases and parts of the stack.
func (r *stackRun) nodes() (map[string]*node, hcl.Diagnostics) {
	nodes, diags := localNodes(r.stack.Locals)

	for _, rel := range r.stack.Releases {
		key := moduleRoot + "." + rel.Name
		if _, dup := nodes[key]; dup {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Duplicate release",
				Detail: fmt.Sprintf("The release %q is in the stack twice.", rel.Name), Subject: rel.Block.DefRange.Ptr(),
			})

			continue
		}

		nodes[key] = &node{key: key, release: rel, rng: rel.Block.LabelRanges[0]}
	}

	for _, p := range r.stack.Parts {
		key := stackRoot + "." + p.Name
		if _, dup := nodes[key]; dup {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Duplicate part",
				Detail: fmt.Sprintf("The part %q is in the stack twice.", p.Name), Subject: p.Block.DefRange.Ptr(),
			})

			continue
		}

		nodes[key] = &node{key: key, part: p, rng: p.Block.LabelRanges[0]}
	}

	for _, rel := range r.stack.Releases {
		for _, dep := range rel.DependsOn {
			if _, ok := nodes[moduleRoot+"."+dep]; !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError, Summary: "Unknown release",
					Detail: fmt.Sprintf("depends_on names module.%s, which the stack does not have.", dep), Subject: rel.Block.DefRange.Ptr(),
				})
			}
		}
	}

	return nodes, diags
}

// values evaluates the locals and the releases in the order of their
// references.
func (r *stackRun) values() hcl.Diagnostics {
	nodes, diags := r.nodes()
	if diags.HasErrors() {
		return diags
	}

	order, moreDiags := orderNodes(nodes)
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return diags
	}

	locals, modules, parts := map[string]cty.Value{}, map[string]cty.Value{}, map[string]cty.Value{}

	for _, key := range order {
		n := nodes[key]
		name := key[strings.IndexByte(key, '.')+1:]

		if n.local != nil {
			v, moreDiags := n.local.Expr.Value(r.base)
			diags = append(diags, moreDiags...)
			locals[name] = v
			r.base.Variables[localsRoot] = cty.ObjectVal(locals)

			continue
		}

		if n.part != nil {
			v, moreDiags := r.part(n)
			diags = append(diags, moreDiags...)
			parts[name] = v
			r.base.Variables[stackRoot] = cty.ObjectVal(parts)

			continue
		}

		rr, moreDiags := r.release(n)
		diags = append(diags, moreDiags...)

		switch {
		case moreDiags.HasErrors():
			modules[name] = cty.DynamicVal
		case !rr.Enabled:
			modules[name] = cty.NullVal(cty.DynamicPseudoType)
		default:
			modules[name] = rr.Result.OutputValues
		}

		r.base.Variables[moduleRoot] = cty.ObjectVal(modules)
	}

	return append(diags, r.uniqueNames()...)
}

// uniqueNames reports two releases of one name: a release of the stack and
// one of a part, <part>-<release>.
func (r *stackRun) uniqueNames() hcl.Diagnostics {
	var diags hcl.Diagnostics

	seen := map[string]bool{}

	for _, rr := range r.res.Releases {
		if seen[rr.Name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Duplicate release",
				Detail:  fmt.Sprintf("Two releases of the stack are named %q: one of a part is named <part>-<release>.", rr.Name),
				Subject: rr.Release.Block.DefRange.Ptr(),
			})
		}

		seen[rr.Name] = true
	}

	return diags
}

// dependencies are the releases a node reads or names in depends_on: the
// releases of the stack, and every release of a part.
func (r *stackRun) dependencies(n *node) []string {
	var deps []string

	for _, ref := range n.refs() {
		if dep, ok := strings.CutPrefix(ref, moduleRoot+"."); ok {
			deps = append(deps, r.prefix+dep)
		} else if part, ok := strings.CutPrefix(ref, stackRoot+"."); ok {
			deps = append(deps, r.partReleases[part]...)
		}
	}

	slices.Sort(deps)

	return slices.Compact(deps)
}

// part evaluates a part of the stack (design 7.5): its published stack
// with the values the stack gives it, in the region of the stack and its
// namespace unless the part names one. Its releases are named
// <part>-<release> and depend on what the part reads; its generated values
// are <part>-<name>. It returns the outputs of the part.
func (r *stackRun) part(n *node) (cty.Value, hcl.Diagnostics) {
	p := n.part
	if p.Stack == nil {
		return cty.DynamicVal, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Part not loaded",
			Detail: fmt.Sprintf("The published stack of the part %q is not loaded; run rigger stack sync.", p.Name), Subject: p.Block.DefRange.Ptr(),
		}}
	}

	inputs := map[string]Input{}

	if p.Values != nil {
		v, diags := p.Values.Value(r.base)
		if diags.HasErrors() {
			return cty.DynamicVal, diags
		}

		rng := p.Values.Range()
		if v.IsNull() || !v.IsWhollyKnown() || !isObject(v) {
			return cty.DynamicVal, hcl.Diagnostics{{
				Severity: hcl.DiagError, Summary: "Invalid values",
				Detail: "The values of a part are an object: one attribute per variable of its stack.", Subject: rng.Ptr(),
			}}
		}

		for name, value := range v.AsValueMap() {
			inputs[name] = Input{Value: value, Range: &rng}
		}
	}

	vars, diags := Variables(p.Stack.Vars, inputs)
	if diags.HasErrors() {
		return cty.DynamicVal, diags
	}

	view := *p.Stack
	view.Rigger.Name, view.Rigger.Region = r.stack.Rigger.Name, StackRegion(r.stack)

	if view.Rigger.Namespace == "" {
		view.Rigger.Namespace = StackNamespace(r.stack)
	}

	sub := newStackRun(r.ctx, &view, vars, func(name string) (string, bool) { return r.generated(p.Name + "-" + name) },
		r.cfg, r.prefix+p.Name+"-")

	res, outputs, moreDiags := sub.run()
	if diags = append(diags, moreDiags...); moreDiags.HasErrors() {
		return cty.DynamicVal, diags
	}

	deps := r.dependencies(n)

	for _, rr := range res.Releases {
		rr.DependsOn = slices.Compact(slices.Sorted(slices.Values(append(rr.DependsOn, deps...))))
		r.partReleases[p.Name] = append(r.partReleases[p.Name], rr.Name)
	}

	r.res.Releases = append(r.res.Releases, res.Releases...)
	r.res.Secrets = append(r.res.Secrets, res.Secrets...)

	for name := range sub.used {
		r.used[p.Name+"-"+name] = true
	}

	return outputs, diags
}

// release evaluates a release of the stack.
func (r *stackRun) release(n *node) (*ReleaseResult, hcl.Diagnostics) {
	rel := n.release
	rr := &ReleaseResult{Release: rel, Name: r.prefix + rel.Name, Enabled: true, Namespace: StackNamespace(r.stack), DependsOn: r.dependencies(n)}
	r.res.Releases = append(r.res.Releases, rr)

	if len(rr.Name) > maxReleaseName {
		return rr, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Release name too long",
			Detail:  fmt.Sprintf("The release %q is longer than %d characters.", rr.Name, maxReleaseName),
			Subject: rel.Block.DefRange.Ptr(),
		}}
	}

	diags := r.settings(rr)
	if diags.HasErrors() || !rr.Enabled {
		return rr, diags
	}

	inputs, values, moreDiags := r.inputs(rr)
	diags = append(diags, moreDiags...)

	if diags.HasErrors() {
		return rr, diags
	}

	rr.Values = MarkSensitive(rel.Module, values)

	vars, moreDiags := Variables(rel.Module, inputs)
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return rr, diags
	}

	md := rel.Module.Metadata
	facts := Facts{
		Release: rr.Name, Namespace: rr.Namespace, Region: StackRegion(r.stack),
		ModuleName: md.Name, ModuleVersion: md.Version,
	}

	res, moreDiags := Evaluate(r.ctx, rel.Module, vars, facts, r.cfg)
	diags = append(diags, moreDiags...)
	rr.Result = res

	return rr, diags
}

// settings evaluates enabled and namespace of a release.
func (r *stackRun) settings(rr *ReleaseResult) hcl.Diagnostics {
	rel := rr.Release

	if rel.Enabled != nil {
		var enabled bool

		v, diags := rel.Enabled.Value(r.base)
		if diags.HasErrors() {
			return diags
		}

		if err := toGo(v, &enabled); err != nil {
			return hcl.Diagnostics{{
				Severity: hcl.DiagError, Summary: "Invalid enabled",
				Detail: "enabled of a release is true or false.", Subject: rel.Enabled.Range().Ptr(),
			}}
		}

		rr.Enabled = enabled
	}

	if rel.Namespace != nil {
		var ns string

		v, diags := rel.Namespace.Value(r.base)
		if diags.HasErrors() {
			return diags
		}

		if err := toGo(v, &ns); err != nil || ns == "" {
			return hcl.Diagnostics{{
				Severity: hcl.DiagError, Summary: "Invalid namespace",
				Detail: "namespace of a release is a name.", Subject: rel.Namespace.Range().Ptr(),
			}}
		}

		rr.Namespace = ns
	}

	return nil
}

// inputs evaluates the values of a release, with the generated values in
// them, as the inputs of its module.
func (r *stackRun) inputs(rr *ReleaseResult) (map[string]Input, cty.Value, hcl.Diagnostics) {
	rel := rr.Release
	if rel.Values == nil {
		return nil, cty.EmptyObjectVal, nil
	}

	v, diags := rel.Values.Value(r.base)
	if diags.HasErrors() {
		return nil, cty.NilVal, diags
	}

	rng := rel.Values.Range()

	if v.IsNull() || !v.IsWhollyKnown() || (!isObject(v)) {
		return nil, cty.NilVal, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Invalid values",
			Detail: "The values of a release are an object: one attribute per variable of its module.", Subject: rng.Ptr(),
		}}
	}

	v, pending, err := r.forms(v)
	if err != nil {
		return nil, cty.NilVal, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: summaryInvalidValue, Detail: err.Error() + ".", Subject: rng.Ptr()}}
	}

	rr.Pending = pending

	inputs := map[string]Input{}
	for name, value := range v.AsValueMap() {
		inputs[name] = Input{Value: value, Range: &rng}
	}

	return inputs, v, nil
}

// forms replaces the value forms of v: a string "!generate:<name>" is the
// generated value of that name, sensitive. pending reports a value that is
// not generated yet; it stands as the empty string until apply.
func (r *stackRun) forms(v cty.Value) (cty.Value, bool, error) {
	pending := false

	out, err := cty.Transform(v, func(_ cty.Path, v cty.Value) (cty.Value, error) {
		inner, marks := v.Unmark()
		if inner.IsNull() || !inner.IsKnown() || inner.Type() != cty.String {
			return v, nil
		}

		name, ok := strings.CutPrefix(inner.AsString(), generatePrefix)
		if !ok {
			return v, nil
		}

		if !validGeneratedName(name) {
			return cty.NilVal, fmt.Errorf("%q is not a name of a generated value: letters, digits, - and _", name)
		}

		r.used[name] = true

		value, ok := r.generated(name)
		if !ok {
			pending = true
		}

		return cty.StringVal(value).WithMarks(marks).Mark(Sensitive), nil
	})

	return out, pending, err
}

func validGeneratedName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}

	for _, c := range name {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}

	return true
}

// secrets evaluates the secret blocks: secret "<path>" { items, overwrite }.
func (r *stackRun) secrets() hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, b := range r.stack.Secrets {
		var s struct {
			Items     hcl.Expression `hcl:"items"`
			Overwrite *bool          `hcl:"overwrite,optional"`
		}

		if moreDiags := decodeBody(b, r.base, &s); moreDiags.HasErrors() {
			diags = append(diags, moreDiags...)

			continue
		}

		items, moreDiags := r.secretItems(s.Items)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			continue
		}

		secret := StackSecret{Path: b.Labels[0], Items: map[string]string{}, Overwrite: s.Overwrite != nil && *s.Overwrite}

		for name, v := range items {
			v, _ = v.UnmarkDeep()
			secret.Items[name] = v.AsString()
		}

		r.res.Secrets = append(r.res.Secrets, secret)
	}

	return diags
}

// secretItems evaluates the items of a secret: strings, value forms
// replaced.
func (r *stackRun) secretItems(expr hcl.Expression) (map[string]cty.Value, hcl.Diagnostics) {
	v, diags := expr.Value(r.base)
	if diags.HasErrors() {
		return nil, diags
	}

	rng := expr.Range()

	if v.IsNull() || !isObject(v) {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid items", Detail: "The items of a secret are an object of strings.", Subject: rng.Ptr()}}
	}

	v, _, err := r.forms(v)
	if err != nil {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: summaryInvalidValue, Detail: err.Error() + ".", Subject: rng.Ptr()}}
	}

	items := map[string]cty.Value{}

	for name, item := range v.AsValueMap() {
		s, err := convert.Convert(item, cty.String)
		if err != nil || s.IsNull() {
			return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid items", Detail: fmt.Sprintf("The item %s of a secret is not a string.", name), Subject: rng.Ptr()}}
		}

		items[name] = s
	}

	return items, nil
}

// TrustRule is a trust block of a stack (design 8.2): the keys a module of
// a registry, or of any source without a rule of its own, must be signed
// with.
type TrustRule struct {
	// Registry is the registry the rule is for; empty for the default.
	Registry string
	// Require refuses a module that no key signed.
	Require bool
	// Keys are the PEM of the public keys.
	Keys []string
}

// StackTrust evaluates the trust blocks of a stack: registry,
// require_signature, public_keys (file() reads the files of the stack).
func StackTrust(s *lang.Stack, fileBytes int64) ([]TrustRule, hcl.Diagnostics) {
	ctx := &hcl.EvalContext{Functions: moduleFunctions(s.FS, fileBytes)}

	var (
		rules []TrustRule
		diags hcl.Diagnostics
	)

	for _, b := range s.Rigger.Trust {
		var t struct {
			Registry string   `hcl:"registry,optional"`
			Require  bool     `hcl:"require_signature,optional"`
			Keys     []string `hcl:"public_keys,optional"`
		}

		if moreDiags := decodeBody(b, ctx, &t); moreDiags.HasErrors() {
			diags = append(diags, moreDiags...)

			continue
		}

		if slices.ContainsFunc(rules, func(r TrustRule) bool { return r.Registry == t.Registry }) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Duplicate trust",
				Detail: fmt.Sprintf("The stack has two trust blocks for the registry %q.", t.Registry), Subject: b.DefRange.Ptr(),
			})

			continue
		}

		rules = append(rules, TrustRule{Registry: t.Registry, Require: t.Require, Keys: t.Keys})
	}

	return rules, diags
}
