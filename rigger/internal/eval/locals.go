// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// The roots of the references a module evaluates (design 4.2), and the
// attributes of each.* and of an iterator.
const (
	varRoot    = "var"
	localsRoot = "local"
	moduleRoot = "module"
	riggerRoot = "rigger"
	eachRoot   = "each"
	attrKey    = "key"
	attrValue  = "value"
	// stackRoot holds the outputs of the parts of a stack (design 7.5).
	stackRoot = "stack"
)

// node is a value of a module or a stack that others may read: a local, a
// child module, or a release of a stack. Its key is local.<name> or
// module.<name>.
type node struct {
	key     string
	local   *hcl.Attribute
	child   *lang.Child
	release *lang.StackRelease
	part    *lang.StackPart
	rng     hcl.Range
}

// refs returns the keys of the nodes the node reads.
func (n *node) refs() []string {
	var out []string

	switch {
	case n.local != nil:
		return valueRefs(n.local.Expr)
	case n.child != nil:
		for _, attr := range n.child.Args {
			out = append(out, valueRefs(attr.Expr)...)
		}
	case n.part != nil:
		if n.part.Values != nil {
			out = append(out, valueRefs(n.part.Values)...)
		}

		for _, dep := range n.part.DependsOn {
			out = append(out, moduleRoot+"."+dep)
		}

		for _, dep := range n.part.DependsOnParts {
			out = append(out, stackRoot+"."+dep)
		}
	default:
		for _, e := range []hcl.Expression{n.release.Values, n.release.Enabled, n.release.Namespace} {
			if e != nil {
				out = append(out, valueRefs(e)...)
			}
		}

		for _, dep := range n.release.DependsOn {
			out = append(out, moduleRoot+"."+dep)
		}

		for _, dep := range n.release.DependsOnParts {
			out = append(out, stackRoot+"."+dep)
		}
	}

	return out
}

// allValues stands for every value of a root in the key of a reference:
// local.*.
const allValues = "*"

// valueRefs returns the keys of the locals and child modules an expression
// reads. A value read by its name gives its key, whether the name is
// written as an attribute (local.a) or as an index (local["a"]). A root
// read in another way, as a whole (keys(local)) or with a name that is
// computed (local[var.name]), reads every value of the root: the key is the
// root with allValues, which reads resolves.
func valueRefs(expr hcl.Expression) []string {
	var refs []string

	for _, t := range expr.Variables() {
		root := t.RootName()
		if root != localsRoot && root != moduleRoot && root != stackRoot {
			continue
		}

		refs = append(refs, root+"."+refName(t))
	}

	return refs
}

// refName is the name of the value a reference reads of its root, or
// allValues.
func refName(t hcl.Traversal) string {
	if len(t) < 2 {
		return allValues
	}

	switch step := t[1].(type) {
	case hcl.TraverseAttr:
		return step.Name
	case hcl.TraverseIndex:
		if name, ok := stringValue(step.Key); ok {
			return name
		}
	}

	return allValues
}

// reads returns the keys of the nodes the node at key reads. A root read
// as a whole is every other node of the root, by name: a value does not
// wait for itself to read the others. A reference to no node is left to
// the evaluation, which reports it.
func reads(nodes map[string]*node, key string) []string {
	var out []string

	for _, ref := range nodes[key].refs() {
		root, name, _ := strings.Cut(ref, ".")

		if name != allValues {
			if _, ok := nodes[ref]; ok {
				out = append(out, ref)
			}

			continue
		}

		var all []string

		for other := range nodes {
			if other != key && strings.HasPrefix(other, root+".") {
				all = append(all, other)
			}
		}

		slices.Sort(all)

		out = append(out, all...)
	}

	return out
}

// valueNodes gathers the locals of the locals blocks and the child modules.
func valueNodes(blocks []*hcl.Block, children []*lang.Child) (map[string]*node, hcl.Diagnostics) {
	nodes, diags := localNodes(blocks)

	for _, c := range children {
		key := moduleRoot + "." + c.Name
		nodes[key] = &node{key: key, child: c, rng: c.Block.LabelRanges[0]}
	}

	return nodes, diags
}

// localNodes gathers the locals of the locals blocks.
func localNodes(blocks []*hcl.Block) (map[string]*node, hcl.Diagnostics) {
	nodes := map[string]*node{}

	var diags hcl.Diagnostics

	for _, b := range blocks {
		attrs, moreDiags := b.Body.JustAttributes()
		diags = append(diags, moreDiags...)

		for name, attr := range attrs {
			key := localsRoot + "." + name
			if prev, dup := nodes[key]; dup {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError, Summary: "Duplicate local",
					Detail:  fmt.Sprintf("%s is defined already at %s.", key, prev.rng),
					Subject: attr.NameRange.Ptr(),
				})

				continue
			}

			nodes[key] = &node{key: key, local: attr, rng: attr.NameRange}
		}
	}

	return nodes, diags
}

// orderNodes returns the keys of the nodes so that each comes after the
// nodes it reads, or an error naming a cycle. The order of the files and of
// the attributes has no meaning (design 4.1).
func orderNodes(nodes map[string]*node) ([]string, hcl.Diagnostics) {
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	// The states of a node in the walk; zero is not visited yet.
	const (
		visiting = iota + 1
		done
	)

	state := map[string]int{}
	order := make([]string, 0, len(keys))

	var visit func(key string, path []string) hcl.Diagnostics

	visit = func(key string, path []string) hcl.Diagnostics {
		switch state[key] {
		case done:
			return nil
		case visiting:
			cycle := append(slices.Clone(path[slices.Index(path, key):]), key)

			return hcl.Diagnostics{{
				Severity: hcl.DiagError, Summary: "Values refer to each other",
				Detail:  "The locals and modules form a cycle: " + strings.Join(cycle, " → ") + ".",
				Subject: nodes[key].rng.Ptr(),
			}}
		}

		state[key] = visiting

		for _, dep := range reads(nodes, key) {
			if diags := visit(dep, append(path, key)); diags.HasErrors() {
				return diags
			}
		}

		state[key] = done
		order = append(order, key)

		return nil
	}

	for _, key := range keys {
		if diags := visit(key, nil); diags.HasErrors() {
			return nil, diags
		}
	}

	return order, nil
}
