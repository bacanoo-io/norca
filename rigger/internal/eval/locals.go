// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// localsRoot is the root name of a reference to a local.
const localsRoot = "local"

// evalLocals evaluates the locals of the blocks in the order of their
// references, and returns the object local.*. The order of the files and of
// the attributes has no meaning (design 4.1).
func evalLocals(blocks []*hcl.Block, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	attrs, diags := localAttributes(blocks)
	if diags.HasErrors() {
		return cty.EmptyObjectVal, diags
	}

	order, moreDiags := orderLocals(attrs)
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return cty.EmptyObjectVal, diags
	}

	values := map[string]cty.Value{}
	child := ctx.NewChild()

	for _, name := range order {
		child.Variables = map[string]cty.Value{localsRoot: cty.ObjectVal(values)}

		v, moreDiags := attrs[name].Expr.Value(child)
		diags = append(diags, moreDiags...)
		values[name] = v
	}

	return cty.ObjectVal(values), diags
}

// localAttributes gathers the attributes of all locals blocks.
func localAttributes(blocks []*hcl.Block) (map[string]*hcl.Attribute, hcl.Diagnostics) {
	attrs := map[string]*hcl.Attribute{}

	var diags hcl.Diagnostics

	for _, b := range blocks {
		more, moreDiags := b.Body.JustAttributes()
		diags = append(diags, moreDiags...)

		for name, attr := range more {
			if prev, dup := attrs[name]; dup {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError, Summary: "Duplicate local",
					Detail:  fmt.Sprintf("local.%s is defined already at %s.", name, prev.NameRange),
					Subject: attr.NameRange.Ptr(),
				})

				continue
			}

			attrs[name] = attr
		}
	}

	return attrs, diags
}

// orderLocals returns the names of the locals so that each comes after the
// locals it reads, or an error naming a cycle.
func orderLocals(attrs map[string]*hcl.Attribute) ([]string, hcl.Diagnostics) {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}

	slices.Sort(names)

	// The states of a local in the walk; zero is not visited yet.
	const (
		visiting = iota + 1
		done
	)

	state := map[string]int{}
	order := make([]string, 0, len(names))

	var visit func(name string, path []string) hcl.Diagnostics

	visit = func(name string, path []string) hcl.Diagnostics {
		switch state[name] {
		case done:
			return nil
		case visiting:
			cycle := append(slices.Clone(path[slices.Index(path, name):]), name)

			return hcl.Diagnostics{{
				Severity: hcl.DiagError, Summary: "Locals refer to each other",
				Detail:  "The locals form a cycle: local." + strings.Join(cycle, " → local.") + ".",
				Subject: attrs[name].NameRange.Ptr(),
			}}
		}

		state[name] = visiting

		for _, dep := range localRefs(attrs[name].Expr) {
			if _, ok := attrs[dep]; !ok {
				continue
			}

			if diags := visit(dep, append(path, name)); diags.HasErrors() {
				return diags
			}
		}

		state[name] = done
		order = append(order, name)

		return nil
	}

	for _, name := range names {
		if diags := visit(name, nil); diags.HasErrors() {
			return nil, diags
		}
	}

	return order, nil
}

// localRefs returns the names of the locals an expression reads.
func localRefs(expr hcl.Expression) []string {
	var refs []string

	for _, t := range expr.Variables() {
		if t.RootName() != localsRoot || len(t) < 2 {
			continue
		}

		if attr, ok := t[1].(hcl.TraverseAttr); ok {
			refs = append(refs, attr.Name)
		}
	}

	return refs
}
