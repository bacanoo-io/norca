// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/gocty"

	"github.com/bacanoo-io/norca-rig/host"
	"github.com/bacanoo-io/norca-rig/internal/lang"
	"github.com/bacanoo-io/norca-rig/resource"
)

// The meta-arguments Rig adds to a resource (design 5).
const (
	metaEnabled = "enabled"
	metaForEach = "for_each"
	metaCount   = "count"
	// itemsName is the attribute of a Variable that holds its items.
	itemsName = "items"
)

var metaSchema = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{
	{Name: metaEnabled}, {Name: metaForEach}, {Name: metaCount},
}}

var metaNames = map[string]bool{metaEnabled: true, metaForEach: true, metaCount: true}

// evaluator evaluates the resources of one module.
type evaluator struct {
	ctx     context.Context //nolint:containedctx // lives for one call of Evaluate
	cfg     Config
	sources map[string][]byte
	base    *hcl.EvalContext
}

// instance is one copy of a resource: its key and its each.* or count.*.
type instance struct {
	suffix string
	vars   map[string]cty.Value
}

// resource evaluates a resource: none when it is not enabled, one per key
// of its for_each or per index of its count, one otherwise.
func (e *evaluator) resource(r *lang.Resource) ([]Resource, hcl.Diagnostics) {
	meta, remain, diags := r.Block.Body.PartialContent(metaSchema)
	if diags.HasErrors() {
		return nil, diags
	}

	instances, moreDiags := e.instances(meta.Attributes)
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return nil, diags
	}

	var out []Resource

	for _, in := range instances {
		ctx := e.base.NewChild()
		ctx.Variables = in.vars

		res, moreDiags := e.instance(r, remain, ctx)
		diags = append(diags, moreDiags...)

		if !moreDiags.HasErrors() {
			res.Name = r.Name + in.suffix
			out = append(out, res)
		}
	}

	return out, diags
}

// instances returns the copies of a resource its meta-arguments ask for.
func (e *evaluator) instances(attrs hcl.Attributes) ([]instance, hcl.Diagnostics) {
	if attr, ok := attrs[metaEnabled]; ok {
		enabled, diags := e.boolMeta(attr)
		if diags.HasErrors() || !enabled {
			return nil, diags
		}
	}

	forEach, hasForEach := attrs[metaForEach]
	count, hasCount := attrs[metaCount]

	switch {
	case hasForEach && hasCount:
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "for_each and count together",
			Detail: "A resource takes for_each or count, not both.", Subject: count.NameRange.Ptr(),
		}}
	case hasForEach:
		return e.forEachInstances(forEach)
	case hasCount:
		return e.countInstances(count)
	default:
		return []instance{{}}, nil
	}
}

// metaValue evaluates a meta-argument: known, not null, not sensitive.
func (e *evaluator) metaValue(attr *hcl.Attribute) (cty.Value, hcl.Diagnostics) {
	v, diags := attr.Expr.Value(e.base)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}

	switch {
	case v.ContainsMarked():
		return cty.NilVal, hcl.Diagnostics{sensitiveDiag(attr.Name, attr.Expr.Range())}
	case v.IsNull() || !v.IsWhollyKnown():
		return cty.NilVal, hcl.Diagnostics{metaDiag(attr, "a known value, not null")}
	}

	return v, diags
}

func (e *evaluator) boolMeta(attr *hcl.Attribute) (bool, hcl.Diagnostics) {
	v, diags := e.metaValue(attr)
	if diags.HasErrors() {
		return false, diags
	}

	if v.Type() != cty.Bool {
		return false, hcl.Diagnostics{metaDiag(attr, "true or false")}
	}

	return v.True(), diags
}

func (e *evaluator) countInstances(attr *hcl.Attribute) ([]instance, hcl.Diagnostics) {
	v, diags := e.metaValue(attr)
	if diags.HasErrors() {
		return nil, diags
	}

	var n int
	if v.Type() != cty.Number || gocty.FromCtyValue(v, &n) != nil || n < 0 {
		return nil, hcl.Diagnostics{metaDiag(attr, "a whole number, 0 or more")}
	}

	out := make([]instance, n)
	for i := range out {
		out[i] = instance{
			suffix: "[" + strconv.Itoa(i) + "]",
			vars:   map[string]cty.Value{metaCount: cty.ObjectVal(map[string]cty.Value{"index": cty.NumberIntVal(int64(i))})},
		}
	}

	return out, diags
}

// forEachInstances takes a map or an object (each.key is the key, each.value
// the element), or a collection of strings (each.key and each.value are the
// string).
func (e *evaluator) forEachInstances(attr *hcl.Attribute) ([]instance, hcl.Diagnostics) {
	v, diags := e.metaValue(attr)
	if diags.HasErrors() {
		return nil, diags
	}

	ty := v.Type()
	byKey := ty.IsMapType() || ty.IsObjectType()

	if !byKey && !ty.IsSetType() && !ty.IsListType() && !ty.IsTupleType() {
		return nil, hcl.Diagnostics{metaDiag(attr, "a map, an object or a collection of strings")}
	}

	var out []instance

	seen := map[string]bool{}

	for it := v.ElementIterator(); it.Next(); {
		key, value := it.Element()
		if !byKey {
			key = value
		}

		k, ok := stringValue(key)
		if !ok {
			return nil, hcl.Diagnostics{metaDiag(attr, "a map, an object or a collection of strings")}
		}

		if seen[k] {
			return nil, hcl.Diagnostics{metaDiag(attr, fmt.Sprintf("keys written once; %q is there twice", k))}
		}

		seen[k] = true
		out = append(out, instance{
			suffix: "[" + strconv.Quote(k) + "]",
			vars: map[string]cty.Value{"each": cty.ObjectVal(map[string]cty.Value{
				"key": cty.StringVal(k), "value": value,
			})},
		})
	}

	return out, diags
}

func metaDiag(attr *hcl.Attribute, want string) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError, Summary: "Invalid " + attr.Name,
		Detail: fmt.Sprintf("The %s of a resource is %s.", attr.Name, want), Subject: attr.Expr.Range().Ptr(),
	}
}

// instance evaluates one copy of a resource; body is its block without the
// meta-arguments.
func (e *evaluator) instance(r *lang.Resource, body hcl.Body, ctx *hcl.EvalContext) (Resource, hcl.Diagnostics) {
	if r.Type.Kind == resource.KindJob {
		return e.job(r, body, ctx)
	}

	w := &walker{skip: metaNames}

	v, diags := w.value(r.Block.Body, ctx)
	if diags.HasErrors() {
		return Resource{}, diags
	}

	res := Resource{Kind: r.Type.Kind}

	for _, m := range w.marked {
		if r.Type.Kind == resource.KindVariable && m.path[0] == itemsName && len(m.path) <= 2 {
			continue
		}

		diags = append(diags, sensitiveDiag(fmt.Sprintf("a %s, except in its items", r.Type.Kind), m.rng))
	}

	if r.Type.Kind == resource.KindVariable {
		var moreDiags hcl.Diagnostics

		v, res.Secrets, moreDiags = secretItems(v, r.Block.DefRange)
		diags = append(diags, moreDiags...)
	}

	if diags.HasErrors() {
		return Resource{}, diags
	}

	data, err := marshal(v)
	if err != nil {
		return Resource{}, append(diags, valueDiag(string(r.Type.Kind)+" "+r.Name, err, r.Block.DefRange))
	}

	res.Spec = data

	return res, diags
}

// job evaluates a job: the decoder of the host decodes it, with the runtime
// references passed through. A job takes no sensitive value: the decoder
// would write it into the job, which is stored as it is.
func (e *evaluator) job(r *lang.Resource, body hcl.Body, ctx *hcl.EvalContext) (Resource, hcl.Diagnostics) {
	jobCtx := ctx.NewChild()
	jobCtx.UndefinedVariable = passthrough(e.sources)

	// The decoder reports the errors of the job; this walk only finds the
	// sensitive values before the decoder sees them.
	w := &walker{skip: metaNames}
	_, _ = w.value(r.Block.Body, jobCtx)

	var diags hcl.Diagnostics
	for _, m := range w.marked {
		diags = append(diags, sensitiveDiag("a job", m.rng))
	}

	if diags.HasErrors() {
		return Resource{}, diags
	}

	if e.cfg.Jobs == nil {
		return Resource{}, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "No job decoder",
			Detail:  "The engine has no job decoder, so it can't evaluate a job (rig.WithJobDecoder).",
			Subject: r.Block.DefRange.Ptr(),
		}}
	}

	block := *r.Block
	block.Body = body

	job, diags := e.cfg.Jobs.DecodeJob(e.ctx, host.JobSpec{Block: &block, EvalContext: jobCtx})
	if diags.HasErrors() {
		return Resource{}, diags
	}

	return Resource{Kind: resource.KindJob, Spec: job.Spec}, diags
}

// secretItems takes the items of a Variable that hold a sensitive value out
// of v: they are the secret items of the release (design 6.3).
func secretItems(v cty.Value, rng hcl.Range) (cty.Value, map[string]host.Sensitive, hcl.Diagnostics) {
	if !v.Type().IsObjectType() || !v.Type().HasAttribute(itemsName) {
		return v, nil, nil
	}

	items, marks := v.GetAttr(itemsName).Unmark()
	if !items.Type().IsObjectType() && !items.Type().IsMapType() {
		if len(marks) > 0 || items.ContainsMarked() {
			return v, nil, hcl.Diagnostics{sensitiveDiag("items that are not an object", rng)}
		}

		return v, nil, nil
	}

	var (
		secrets map[string]host.Sensitive
		diags   hcl.Diagnostics
	)

	plain := map[string]cty.Value{}

	for name, item := range items.AsValueMap() {
		if len(marks) == 0 && !item.ContainsMarked() {
			plain[name] = item

			continue
		}

		s, ok := secretString(item)
		if !ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Invalid secret item",
				Detail:  fmt.Sprintf("The item %s of a Variable takes a sensitive value that is not a string.", name),
				Subject: rng.Ptr(),
			})

			continue
		}

		if secrets == nil {
			secrets = map[string]host.Sensitive{}
		}

		secrets[name] = s
	}

	attrs := v.AsValueMap()
	attrs[itemsName] = cty.ObjectVal(plain)

	return cty.ObjectVal(attrs), secrets, diags
}

// secretString returns a sensitive item as a secret: a string, or a value
// that converts to one.
func secretString(item cty.Value) (host.Sensitive, bool) {
	item, _ = item.UnmarkDeep()

	s, err := convert.Convert(item, cty.String)
	if err != nil || s.IsNull() || !s.IsKnown() {
		return host.Sensitive{}, false
	}

	return host.NewSensitive(s.AsString()), true
}
