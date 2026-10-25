// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"fmt"
	"sort"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/nomad/api"
)

// validateScalingPolicyDocument checks the attributes of the policy block
// of a group scaling block that have a meaning for the typed scaling policy
// of the built-in autoscaler:
//
//	policy {
//	  evaluation_interval     = "30s"
//	  cooldown                = "2m"
//	  cooldown_on_scale_up    = "1m"
//	  on_check_error          = "ignore"
//	  scale_during_deployment = "up_only"
//
//	  check "cpu" {
//	    metric   = "nomad.cpu"         # or source and query
//	    strategy = "target-value"      # or a strategy "name" { ... } block
//	    target   = 70                  # strategy parameters
//	  }
//	}
//
// The block itself stays an untyped document (api.ScalingPolicy.Policy), so
// jobs keep their form for the external autoscaler; the servers derive the
// typed policy from it. Other attributes are left to the policy's engine.
func validateScalingPolicyDocument(p *api.ScalingPolicy, tgBody hcl.Body, ctx *hcl.EvalContext) hcl.Diagnostics {
	if p == nil || len(p.Policy) == 0 {
		return nil
	}

	policyRange := scalingPolicyRange(tgBody)
	var diags hcl.Diagnostics
	errorf := func(subject *hcl.Range, format string, args ...interface{}) {
		if subject == nil {
			subject = policyRange
		}
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid scaling policy",
			Detail:   fmt.Sprintf(format, args...),
			Subject:  subject,
		})
	}

	// At this point of the decoding the entries of the document are still
	// HCL attributes (nested blocks are tuple expressions); they are
	// evaluated into plain values later. Evaluate them here too, keeping
	// their ranges for the diagnostics.
	doc := make(map[string]interface{}, len(p.Policy))
	ranges := map[string]*hcl.Range{}
	for k, raw := range p.Policy {
		if attr, isAttr := raw.(*hcl.Attribute); isAttr {
			v, d := decodeInterface(attr.Expr, ctx)
			if d.HasErrors() {
				// Reported when the document is evaluated.
				continue
			}
			doc[k] = v
			ranges[k] = attr.Range.Ptr()
			continue
		}
		doc[k] = raw
	}

	// rangeOf is the range of the document entry an error belongs to.
	var rangeOf *hcl.Range
	get := func(m map[string]interface{}, key string) (interface{}, *hcl.Range, bool) {
		raw, ok := m[key]
		if !ok {
			return nil, nil, false
		}
		if rng, ok := ranges[key]; ok && rangeOf == nil {
			return raw, rng, true
		}
		return raw, rangeOf, true
	}
	checkEnum := func(m map[string]interface{}, path, key string, values ...string) {
		raw, rng, ok := get(m, key)
		if !ok {
			return
		}
		if s, isStr := raw.(string); isStr {
			for _, v := range values {
				if s == v {
					return
				}
			}
		}
		errorf(rng, "%s.%s must be one of %q, found %v", path, key, values, raw)
	}
	checkDuration := func(m map[string]interface{}, path, key string, allowInstant bool) {
		if raw, rng, ok := get(m, key); ok {
			if err := checkScalingDuration(raw, allowInstant); err != nil {
				errorf(rng, "%s.%s %s", path, key, err)
			}
		}
	}

	for _, key := range []string{"evaluation_interval", "cooldown", "cooldown_on_scale_up"} {
		checkDuration(doc, "policy", key, false)
	}
	checkEnum(doc, "policy", "on_check_error", "fail", "ignore")
	checkEnum(doc, "policy", "scale_during_deployment", "never", api.ScaleDuringDeploymentUpOnly)

	rangeOf = ranges["check"]
	checks, ok := scalingDocBlocks(doc["check"])
	if !ok {
		errorf(nil, "policy.check must be a list of labeled blocks")
		return diags
	}
	for _, name := range sortedKeys(checks) {
		path := fmt.Sprintf("policy.check[%s]", name)
		c, ok := scalingDocBody(checks[name])
		if !ok {
			errorf(nil, "%s must be a block", path)
			continue
		}

		for _, key := range []string{"source", "metric", "query", "group"} {
			if raw, rng, ok := get(c, key); ok {
				if _, isStr := raw.(string); !isStr {
					errorf(rng, "%s.%s must be a string", path, key)
				}
			}
		}
		checkDuration(c, path, "query_window", true)
		checkDuration(c, path, "query_window_offset", false)
		checkEnum(c, path, "on_error", "fail", "ignore")

		switch raw := c["strategy"].(type) {
		case nil:
			// Required by both engines, but reported by the servers, which
			// know the engine of the policy.
		case string:
			if raw == "" {
				errorf(rangeOf, "%s.strategy must be the name of a strategy", path)
			}
		default:
			strategies, ok := scalingDocBlocks(raw)
			if !ok || len(strategies) != 1 {
				errorf(rangeOf, "%s must have exactly one strategy block", path)
			}
		}
	}

	return diags
}

// scalingPolicyRange returns the range of the policy block of the group's
// scaling block, for diagnostics.
func scalingPolicyRange(tgBody hcl.Body) *hcl.Range {
	content, _, _ := tgBody.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "scaling"}},
	})
	if content == nil || len(content.Blocks) == 0 {
		return nil
	}
	scaling := content.Blocks[0]
	policy, _, _ := scaling.Body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "policy"}},
	})
	if policy != nil && len(policy.Blocks) > 0 {
		return policy.Blocks[0].DefRange.Ptr()
	}
	return scaling.DefRange.Ptr()
}

// checkScalingDuration checks a duration attribute of the policy.
func checkScalingDuration(raw interface{}, allowInstant bool) error {
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("must be a duration string such as \"30s\", found %v", raw)
	}
	if allowInstant && s == "instant" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("must be a duration such as \"30s\", found %q", s)
	}
	if d < 0 {
		return fmt.Errorf("must not be negative, found %q", s)
	}
	return nil
}

// scalingDocBlocks flattens a list of labeled blocks of the decoded policy
// document into a map from label to block content.
func scalingDocBlocks(raw interface{}) (map[string]interface{}, bool) {
	out := map[string]interface{}{}
	switch v := raw.(type) {
	case nil:
		return out, true
	case []map[string]interface{}:
		for _, m := range v {
			for label, content := range m {
				out[label] = content
			}
		}
	case []interface{}:
		for _, item := range v {
			m, ok := item.(map[string]interface{})
			if !ok {
				return nil, false
			}
			for label, content := range m {
				out[label] = content
			}
		}
	default:
		return nil, false
	}
	return out, true
}

// scalingDocBody returns the content of a block of the decoded policy
// document.
func scalingDocBody(raw interface{}) (map[string]interface{}, bool) {
	switch v := raw.(type) {
	case map[string]interface{}:
		return v, true
	case []map[string]interface{}:
		if len(v) == 1 {
			return v[0], true
		}
	case []interface{}:
		if len(v) == 1 {
			m, ok := v[0].(map[string]interface{})
			return m, ok
		}
	}
	return nil, false
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
