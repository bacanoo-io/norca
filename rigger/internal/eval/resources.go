// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/gocty"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/internal/syntax"
	"github.com/bacanoo-io/norca/rigger/resource"
)

// The meta-arguments Rigger adds to a resource (design 5).
const (
	metaEnabled = "enabled"
	metaForEach = "for_each"
	metaCount   = "count"
	metaHook    = "hook"
	// itemsName is the attribute of a Variable that holds its items.
	itemsName = "items"
)

var metaSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{{Name: metaEnabled}, {Name: metaForEach}, {Name: metaCount}},
	Blocks:     []hcl.BlockHeaderSchema{{Type: metaHook}},
}

var metaNames = map[string]bool{metaEnabled: true, metaForEach: true, metaCount: true, metaHook: true}

// evaluator evaluates the resources of one module.
type evaluator struct {
	*run

	sources map[string][]byte
	base    *hcl.EvalContext
}

// instance is one copy of a resource: its key and its each.* or count.*.
type instance struct {
	suffix string
	key    cty.Value
	vars   map[string]cty.Value
}

// resource evaluates a resource: none when it is not enabled, one per key
// of its for_each or per index of its count, one otherwise.
func (e *evaluator) resource(r *lang.Resource) ([]Resource, hcl.Diagnostics) {
	meta, remain, diags := r.Block.Body.PartialContent(metaSchema)
	if diags.HasErrors() {
		return nil, diags
	}

	hook, moreDiags := hookBlock(r, meta.Blocks)
	diags = append(diags, moreDiags...)

	instances, moreDiags := e.instances(meta.Attributes)
	diags = append(diags, moreDiags...)

	if diags.HasErrors() {
		return nil, diags
	}

	var out []Resource

	for _, in := range instances {
		if over := stopped(e.ctx, r.Block.DefRange); over.HasErrors() {
			return out, append(diags, over...)
		}

		ctx := e.base.NewChild()
		ctx.Variables = in.vars

		res, moreDiags := e.instance(r, remain, hook, ctx)
		diags = append(diags, moreDiags...)

		if !moreDiags.HasErrors() {
			res.Name, res.Base, res.Key, res.Block = r.Name+in.suffix, r.Name, in.key, r.Type.Block
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

	if over := e.room(n, attr.Expr.Range()); over.HasErrors() {
		return nil, over
	}

	out := make([]instance, n)
	for i := range out {
		out[i] = instance{
			suffix: "[" + strconv.Itoa(i) + "]",
			key:    cty.NumberIntVal(int64(i)),
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

	if over := e.room(v.LengthInt(), attr.Expr.Range()); over.HasErrors() {
		return nil, over
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
			key:    cty.StringVal(k),
			vars: map[string]cty.Value{eachRoot: cty.ObjectVal(map[string]cty.Value{
				attrKey: cty.StringVal(k), attrValue: value,
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
func (e *evaluator) instance(r *lang.Resource, body hcl.Body, hook *hcl.Block, ctx *hcl.EvalContext) (Resource, hcl.Diagnostics) {
	if r.Type.Kind == resource.KindJob {
		return e.job(r, body, hook, ctx)
	}

	if e.cfg.Decoder != nil && slices.Contains(e.cfg.Decoder.Kinds(), r.Type.Kind) {
		return e.decoded(r, ctx)
	}

	w := &walker{skip: metaNames, expansion: &e.expansion}

	v, diags := w.value(r.Block.Body, ctx)
	if diags.HasErrors() {
		return Resource{}, diags
	}

	res := Resource{Kind: r.Type.Kind}

	if e.cfg.Test {
		tw := &walker{skip: metaNames, test: true}
		res.Value, _ = tw.value(r.Block.Body, ctx)
	}

	for _, m := range w.marked {
		if r.Type.Kind == resource.KindVariable && m.path[0] == itemsName && len(m.path) <= 2 {
			continue
		}

		diags = append(diags, sensitiveDiag(fmt.Sprintf("a %s, except in its items", r.Type.Kind), m.rng))
	}

	var moreDiags hcl.Diagnostics

	v, moreDiags = e.byKind(r, v, &res, ctx)
	diags = append(diags, moreDiags...)

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

// byKind does what a kind asks of the value of its block: the secret items
// of a Variable, the text of a block that holds a document of the host
// (textBlocks).
func (e *evaluator) byKind(r *lang.Resource, v cty.Value, res *Resource, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	if r.Type.Kind == resource.KindVariable {
		v, res.Secrets, diags = secretItems(v, r.Block.DefRange)
	}

	if name, ok := textBlocks[r.Type.Kind]; ok {
		var moreDiags hcl.Diagnostics

		v, moreDiags = textBlock(r.Block.Body, name, v, ctx)
		diags = append(diags, moreDiags...)

		if res.Value != cty.NilVal {
			res.Value, _ = textBlock(r.Block.Body, name, res.Value, ctx)
		}
	}

	return v, diags
}

// decoded evaluates a resource of a kind the host decodes from its block
// written as HCL (host.ResourceDecoder).
func (e *evaluator) decoded(r *lang.Resource, ctx *hcl.EvalContext) (Resource, hcl.Diagnostics) {
	res := Resource{Kind: r.Type.Kind}

	if e.cfg.Test {
		tw := &walker{skip: metaNames, test: true, expansion: &e.expansion}
		res.Value, _ = tw.value(r.Block.Body, ctx)
	}

	src, diags := renderBlock(r.Block, r.Name, ctx, metaNames, &e.expansion)
	if diags.HasErrors() {
		return Resource{}, diags
	}

	// The decoder parses the text again, as deep as the values in it.
	if _, over := syntax.ParseConfig(src, r.Name+".hcl"); over.HasErrors() {
		return Resource{}, over
	}

	spec, moreDiags := e.cfg.Decoder.DecodeResource(e.ctx, r.Type.Kind, r.Name, src)
	diags = append(diags, moreDiags...)

	if diags.HasErrors() {
		return Resource{}, diags
	}

	res.Spec = spec

	return res, diags
}

// job evaluates a job: the decoder of the host decodes it, with the runtime
// references passed through. A job takes no sensitive value: the decoder
// would write it into the job, which is stored as it is.
func (e *evaluator) job(r *lang.Resource, body hcl.Body, hookBlock *hcl.Block, ctx *hcl.EvalContext) (Resource, hcl.Diagnostics) {
	var hook *resource.Hook

	if hookBlock != nil {
		var diags hcl.Diagnostics
		if hook, diags = decodeHook(hookBlock, ctx); diags.HasErrors() {
			return Resource{}, diags
		}
	}

	jobCtx := ctx.NewChild()
	jobCtx.UndefinedVariable = passthrough(e.sources)

	// The decoder reports the errors of the job; this walk finds the
	// sensitive values before the decoder sees them, and gives the value a
	// test reads.
	w := &walker{skip: metaNames, test: e.cfg.Test, expansion: &e.expansion}
	value, _ := w.value(r.Block.Body, jobCtx)

	// Before the decoder expands the same blocks with no bound.
	if over := e.expansion.exceeded(); over.HasErrors() {
		return Resource{}, over
	}

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
			Detail:  "The engine has no job decoder, so it can't evaluate a job (rigger.WithJobDecoder).",
			Subject: r.Block.DefRange.Ptr(),
		}}
	}

	block := *r.Block
	block.Body = jobBody(body, jobCtx)

	job, diags := e.cfg.Jobs.DecodeJob(e.ctx, host.JobSpec{Block: &block, EvalContext: jobCtx, Hook: hook})
	if diags.HasErrors() {
		return Resource{}, diags
	}

	res := Resource{Kind: resource.KindJob, Spec: job.Spec, Hook: hook}
	if e.cfg.Test {
		res.Value = withHook(value, hook)
	}

	return res, diags
}

// secretItems takes the items of a Variable that hold a sensitive value out
// of v: they are the secret items of the release (design 6.3).
func secretItems(v cty.Value, rng hcl.Range) (cty.Value, map[string]host.Sensitive, hcl.Diagnostics) {
	if !v.Type().IsObjectType() || !v.Type().HasAttribute(itemsName) {
		return v, nil, nil
	}

	items, marks := v.GetAttr(itemsName).Unmark()

	if ok, diags := readableItems(items, len(marks) > 0, rng); !ok {
		return v, nil, diags
	}

	var (
		secrets map[string]host.Sensitive
		diags   hcl.Diagnostics
	)

	plain := map[string]cty.Value{}

	for name, item := range items.AsValueMap() {
		// A null item is a value not given, sensitive or not: it is left
		// out, as the items block leaves out an attribute that is null
		// (design 4.1).
		if item.IsNull() {
			continue
		}

		if len(marks) == 0 && !item.ContainsMarked() {
			s, diag := itemString(name, item, rng)
			if diag != nil {
				diags = append(diags, diag)

				continue
			}

			plain[name] = s

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

// readableItems reports whether the items of a Variable are an object or a
// map whose items can be taken one by one. Items not known at install time
// are an error: the items of a value not known can't be read. Items of
// another type are left as they are for the host, unless they are
// sensitive.
func readableItems(items cty.Value, sensitive bool, rng hcl.Range) (bool, hcl.Diagnostics) {
	switch {
	case !items.IsKnown():
		return false, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Invalid items",
			Detail: "The items of the Variable are not known at install time.", Subject: rng.Ptr(),
		}}
	case items.Type().IsObjectType() || items.Type().IsMapType():
		return true, nil
	case sensitive || items.ContainsMarked():
		return false, hcl.Diagnostics{sensitiveDiag("items that are not an object", rng)}
	default:
		return false, nil
	}
}

// itemString returns a plain item of a Variable as Norca stores it, a string:
// a number or a bool becomes its string, as HCL converts a value for a
// string, the same as for a secret item (secretString). A list, a map or an
// object has no string, and the server would refuse the Variable at apply:
// it is an error here, naming the item. A null item does not come here: it
// is left out (secretItems). A value not known yet is left for the host.
func itemString(name string, item cty.Value, rng hcl.Range) (cty.Value, *hcl.Diagnostic) {
	if !item.IsKnown() {
		return item, nil
	}

	if err := checkNumbers(item); err != nil {
		return cty.NilVal, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Invalid item",
			Detail: fmt.Sprintf("The item %s of the Variable has no text: %s.", name, err), Subject: rng.Ptr(),
		}
	}

	if item.Type().IsPrimitiveType() {
		if s, err := convert.Convert(item, cty.String); err == nil {
			return s, nil
		}
	}

	return cty.NilVal, &hcl.Diagnostic{
		Severity: hcl.DiagError, Summary: "Invalid item",
		Detail: fmt.Sprintf("The item %s of the Variable is %s; an item of a Variable is a string "+
			"(a number or a bool becomes one).", name, article(item.Type().FriendlyName())),
		Subject: rng.Ptr(),
	}
}

// article puts "a" or "an" before a name.
func article(name string) string {
	if name != "" && strings.ContainsRune("aeiou", rune(name[0])) {
		return "an " + name
	}

	return "a " + name
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
