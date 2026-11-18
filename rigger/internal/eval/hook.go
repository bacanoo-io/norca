// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"

	"github.com/bacanoo-io/norca-rig/internal/lang"
	"github.com/bacanoo-io/norca-rig/resource"
)

// hookBlock returns the hook block of a resource: one at most, on a job
// only (design 5).
func hookBlock(r *lang.Resource, blocks hcl.Blocks) (*hcl.Block, hcl.Diagnostics) {
	switch {
	case len(blocks) == 0:
		return nil, nil
	case r.Type.Kind != resource.KindJob:
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Hook on a resource that is not a job",
			Detail: "A hook is a job with a hook block, not a resource of kind " + string(r.Type.Kind) + ".", Subject: blocks[0].DefRange.Ptr(),
		}}
	case len(blocks) > 1:
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Duplicate hook block",
			Detail: "A job has one hook block.", Subject: blocks[1].DefRange.Ptr(),
		}}
	default:
		return blocks[0], nil
	}
}

// decodeHook decodes a hook block: on (a phase or a list of them), weight,
// timeout and delete.
func decodeHook(b *hcl.Block, ctx *hcl.EvalContext) (*resource.Hook, hcl.Diagnostics) {
	var h struct {
		On      hcl.Expression `hcl:"on"`
		Weight  *int           `hcl:"weight,optional"`
		Timeout *string        `hcl:"timeout,optional"`
		Delete  *string        `hcl:"delete,optional"`
	}

	if diags := gohcl.DecodeBody(b.Body, ctx, &h); diags.HasErrors() {
		return nil, diags
	}

	on, diags := hookPhases(h.On, ctx)
	if diags.HasErrors() {
		return nil, diags
	}

	hook := &resource.Hook{On: on}

	if h.Weight != nil {
		hook.Weight = *h.Weight
	}

	if h.Timeout != nil {
		if _, err := time.ParseDuration(*h.Timeout); err != nil {
			return nil, hcl.Diagnostics{hookDiag(b.DefRange, fmt.Sprintf("timeout %q is not a duration such as \"5m\".", *h.Timeout))}
		}

		hook.Timeout = *h.Timeout
	}

	if h.Delete != nil {
		if !slices.Contains(resource.HookDeletes(), *h.Delete) {
			return nil, hcl.Diagnostics{hookDiag(b.DefRange, fmt.Sprintf("delete is one of %s, not %q.",
				strings.Join(resource.HookDeletes(), ", "), *h.Delete))}
		}

		hook.Delete = *h.Delete
	}

	return hook, nil
}

// hookPhases evaluates on: a phase or a list of phases.
func hookPhases(expr hcl.Expression, ctx *hcl.EvalContext) ([]string, hcl.Diagnostics) {
	v, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, diags
	}

	if v.Type() == cty.String {
		v = cty.ListVal([]cty.Value{v})
	}

	list, err := convert.Convert(v, cty.List(cty.String))
	if err != nil || list.IsNull() || !list.IsWhollyKnown() || list.LengthInt() == 0 {
		return nil, hcl.Diagnostics{hookDiag(expr.Range(), "on is a phase or a list of phases.")}
	}

	var on []string

	for _, p := range list.AsValueSlice() {
		phase := p.AsString()
		if !slices.Contains(resource.HookPhases(), phase) {
			return nil, hcl.Diagnostics{hookDiag(expr.Range(), fmt.Sprintf("%q is not a phase: %s.",
				phase, strings.Join(resource.HookPhases(), ", ")))}
		}

		on = append(on, phase)
	}

	return on, nil
}

func hookDiag(rng hcl.Range, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Invalid hook", Detail: detail, Subject: rng.Ptr()}
}

// withHook adds the hook of a job to its value, as a test reads it.
func withHook(v cty.Value, h *resource.Hook) cty.Value {
	if h == nil || !v.Type().IsObjectType() {
		return v
	}

	on := make([]cty.Value, len(h.On))
	for i, p := range h.On {
		on[i] = cty.StringVal(p)
	}

	attrs := v.AsValueMap()
	if attrs == nil {
		attrs = map[string]cty.Value{}
	}

	attrs[metaHook] = cty.ObjectVal(map[string]cty.Value{
		"on":      cty.ListVal(on),
		"weight":  cty.NumberIntVal(int64(h.Weight)),
		"timeout": cty.StringVal(h.Timeout),
		"delete":  cty.StringVal(h.Delete),
	})

	return cty.ObjectVal(attrs)
}
