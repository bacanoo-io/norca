// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"

	"github.com/agext/levenshtein"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/runref"
)

// namespaces are the roots of the references Rigger resolves at install time
// (design 4.2).
var namespaces = []string{varRoot, localsRoot, riggerRoot, moduleRoot, eachRoot, metaCount}

// passthrough returns the UndefinedVariable callback of the context of a
// job: a reference whose root Rigger does not resolve is a runtime reference,
// given back as a runtime part (package runref), which the job decoder
// writes as ${...} in an attribute of the job and as {{ env "..." }} in the
// data of a task template, so that the cluster resolves it on the node. A root one letter away from a
// namespace of Rigger (vars, locals, riggers) is a mistake, not a runtime
// reference: it is an error with a hint.
func passthrough(sources map[string][]byte) func(hcl.Traversal) (cty.Value, hcl.Diagnostics) {
	return func(t hcl.Traversal) (cty.Value, hcl.Diagnostics) {
		root := t.RootName()
		rng := t.SourceRange()

		for _, ns := range namespaces {
			switch levenshtein.Distance(root, ns, nil) {
			case 0:
				// each and count outside for_each and count.
				return cty.DynamicVal, hcl.Diagnostics{{
					Severity: hcl.DiagError,
					Summary:  "Reference not available here",
					Detail:   fmt.Sprintf("%s.* is defined only inside a resource with %s.", root, metaFor(root)),
					Subject:  rng.Ptr(),
				}}
			case 1:
				return cty.DynamicVal, hcl.Diagnostics{{
					Severity: hcl.DiagError,
					Summary:  "Unknown reference",
					Detail: fmt.Sprintf("%s is neither a reference of Rigger nor one the cluster resolves. Did you mean %s?",
						root, ns),
					Subject: rng.Ptr(),
				}}
			}
		}

		src, ok := sources[rng.Filename]
		if !ok || rng.End.Byte > len(src) {
			return cty.DynamicVal, hcl.Diagnostics{{
				Severity: hcl.DiagError,
				Summary:  "Runtime reference not kept",
				Detail:   "The text of the reference is not known, so it can't be passed to the cluster.",
				Subject:  rng.Ptr(),
			}}
		}

		ref := runref.Part{Kind: runref.KindEnv, Name: string(src[rng.Start.Byte:rng.End.Byte])}

		return cty.StringVal(ref.String()), nil
	}
}

// metaFor names the meta-argument that defines a namespace.
func metaFor(root string) string {
	if root == metaCount {
		return metaCount
	}

	return metaForEach
}
