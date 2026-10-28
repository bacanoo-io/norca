// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package variables

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/hashicorp/nomad/pack/sdk/pack"
	"github.com/zclconf/go-cty/cty"
)

// secretVarName matches variable names that hold secrets or credentials.
var secretVarName = regexp.MustCompile(`(?i)(password|passwd|secret|token|credential|private|api_?key|access_?key)`)

// SynthesizeUIFormSchema builds a unified Draft 2020-12 JSON Schema annotated
// with x-ui-* directives (tabs, groups, widgets, order, defaults, required)
// from the parsed variables of a pack: their types, defaults and ui and
// secret blocks, the tabs of the ui block of metadata.hcl, and the optional
// JSON Schema document (metadata.hcl variables_schema), laid over last.
func SynthesizeUIFormSchema(
	rootID pack.ID,
	packTitle string,
	vars map[pack.ID]map[ID]*Variable,
	tabs []any,
	rawSchemas map[string]string,
) map[string]any {
	out := map[string]any{
		"$schema":    "https://json-schema.org/draft/2020-12/schema",
		"type":       "object",
		"title":      packTitle,
		"properties": map[string]any{},
	}

	rootVars := vars[rootID]
	props := map[string]any{}
	var required []string

	// Sort variable names for deterministic ordering when x-ui-order is not set.
	names := make([]string, 0, len(rootVars))
	for id := range rootVars {
		names = append(names, string(id))
	}
	sort.Strings(names)

	for idx, name := range names {
		v := rootVars[ID(name)]
		if v == nil {
			continue
		}
		prop := ctyVariableToSchemaProperty(name, v, (idx+1)*10)
		props[name] = prop
		if !v.HasDefault() || v.Default == cty.NilVal || v.Default.IsNull() {
			required = append(required, name)
		}
	}

	// Include child dependency variables under "<dep>.<var>" if present.
	depIDs := make([]string, 0, len(vars))
	for pid := range vars {
		if pid != rootID {
			depIDs = append(depIDs, string(pid))
		}
	}
	sort.Strings(depIDs)
	for _, depID := range depIDs {
		depVars := vars[pack.ID(depID)]
		_, shortDep, ok := strings.Cut(depID, ".")
		if !ok {
			shortDep = depID
		}
		depVarNames := make([]string, 0, len(depVars))
		for id := range depVars {
			depVarNames = append(depVarNames, string(id))
		}
		sort.Strings(depVarNames)
		for idx, name := range depVarNames {
			v := depVars[ID(name)]
			if v == nil {
				continue
			}
			key := shortDep + "." + name
			prop := ctyVariableToSchemaProperty(name, v, 500+(idx+1)*10)
			prop["x-ui-tab"] = "dependencies"
			prop["x-ui-group"] = HumanizeIdentifier(shortDep)
			props[key] = prop
			if !v.HasDefault() || v.Default == cty.NilVal || v.Default.IsNull() {
				required = append(required, key)
			}
		}
	}

	out["properties"] = props
	if len(required) > 0 {
		out["required"] = required
	}
	if len(tabs) > 0 {
		out["x-ui-tabs"] = tabs
	}

	// Merge the root pack's explicit JSON Schema (values.schema.json) on top of
	// the extracted variable properties.
	if raw, ok := rawSchemas[string(rootID)]; ok && strings.TrimSpace(raw) != "" {
		var explicit map[string]any
		if err := json.Unmarshal([]byte(raw), &explicit); err == nil {
			mergeSchemaOverlay(out, explicit)
		}
	}

	ensureUITabs(out)
	return out
}

// ConstraintKeys are the JSON Schema keywords of the validation blocks and
// ui options of variables: a pack without a variables_schema whose
// variables have one is validated with its synthesized schema.
var ConstraintKeys = []string{"minimum", "maximum", "pattern", "minLength", "maxLength", "minItems", "maxItems", "uniqueItems", "items", "enum", "type"}

// HasConstraints reports whether a variable has a constraint keyword.
func (v *Variable) HasConstraints() bool {
	for _, k := range ConstraintKeys {
		if _, ok := v.UI[k]; ok {
			return true
		}
	}
	return false
}

func ctyVariableToSchemaProperty(name string, v *Variable, defaultOrder int) map[string]any {
	// Untyped variables take the type of their default; without one any
	// value is accepted.
	t := v.Type
	if (t == cty.NilType || t == cty.DynamicPseudoType) && v.HasDefault() && v.Default != cty.NilVal && !v.Default.IsNull() {
		t = v.Default.Type()
	}
	var prop map[string]any
	if t == cty.NilType || t == cty.DynamicPseudoType {
		prop = map[string]any{"x-ui-widget": "text"}
	} else {
		prop = ctyTypeToSchema(name, t)
	}
	prop["title"] = HumanizeIdentifier(name)
	if v.Description != "" {
		prop["description"] = v.Description
	}
	if v.HasDefault() && v.Default != cty.NilVal && !v.Default.IsNull() {
		if defVal, err := ConvertCtyToInterface(v.Default); err == nil && defVal != nil {
			prop["default"] = defVal
		}
	}
	prop["x-ui-order"] = defaultOrder
	tab, group := inferTabAndGroup(name, prop)
	prop["x-ui-tab"] = tab
	prop["x-ui-group"] = group
	if secretVarName.MatchString(name) && prop["type"] == "string" {
		prop["x-ui-widget"] = "secret"
		prop["x-ui-tab"] = "secrets"
		prop["x-ui-group"] = "Credentials & Secrets"
	}
	applyVariableUI(prop, v.UI)
	return prop
}

// applyVariableUI lays the ui and secret blocks of a variable over the
// inferred property.
func applyVariableUI(prop, ui map[string]any) {
	if len(ui) == 0 {
		return
	}
	_, hasWidget := ui["x-ui-widget"]
	_, hasTab := ui["x-ui-tab"]
	if _, ok := ui["x-ui-secret-path"]; ok {
		if !hasWidget {
			prop["x-ui-widget"] = "secret"
		}
		if !hasTab {
			prop["x-ui-tab"] = "secrets"
			if _, ok := ui["x-ui-group"]; !ok {
				prop["x-ui-group"] = "Credentials & Secrets"
			}
		}
	}
	// Placed on another tab, the inferred group does not apply.
	if _, hasGroup := ui["x-ui-group"]; hasTab && !hasGroup && ui["x-ui-tab"] != prop["x-ui-tab"] {
		delete(prop, "x-ui-group")
	}
	for k, v := range ui {
		// Item constraints add to the item schema of the type.
		if sub, ok := v.(map[string]any); ok && k == "items" {
			if base, ok := prop["items"].(map[string]any); ok {
				merged := maps.Clone(base)
				for sk, sv := range sub {
					merged[sk] = sv
				}
				prop[k] = merged
				continue
			}
		}
		prop[k] = v
	}
	// Options pick a choice widget unless one is given.
	if enum, ok := ui["enum"].([]any); ok && !hasWidget {
		if len(enum) <= 3 {
			prop["x-ui-widget"] = "segmented"
		} else {
			prop["x-ui-widget"] = "select"
		}
	}
}

func ctyTypeToSchema(name string, t cty.Type) map[string]any {
	lower := strings.ToLower(name)
	if t == cty.NilType || t == cty.DynamicPseudoType {
		return map[string]any{"type": "string", "x-ui-widget": inferStringWidget(lower)}
	}
	switch {
	case t.IsPrimitiveType():
		switch t {
		case cty.String:
			return map[string]any{"type": "string", "x-ui-widget": inferStringWidget(lower)}
		case cty.Number:
			return map[string]any{"type": "number", "x-ui-widget": "number"}
		case cty.Bool:
			return map[string]any{"type": "boolean", "x-ui-widget": "switch"}
		}
	case t.IsListType(), t.IsSetType():
		elem := t.ElementType()
		if elem.IsObjectType() || elem.IsMapType() {
			return map[string]any{
				"type":        "array",
				"items":       ctyTypeToSchema(name+"_item", elem),
				"x-ui-widget": "object-list",
			}
		}
		widget := "tags"
		if lower == "datacenters" || strings.HasSuffix(lower, "_datacenters") {
			widget = "datacenter-multiselect"
		}
		return map[string]any{
			"type":        "array",
			"items":       ctyTypeToSchema(name+"_item", elem),
			"x-ui-widget": widget,
		}
	case t.IsTupleType():
		elems := t.TupleElementTypes()
		if len(elems) > 0 && (elems[0].IsObjectType() || elems[0].IsMapType()) {
			return map[string]any{
				"type":        "array",
				"items":       ctyTypeToSchema(name+"_item", elems[0]),
				"x-ui-widget": "object-list",
			}
		}
		return map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"x-ui-widget": "tags",
		}
	case t.IsMapType():
		elem := t.ElementType()
		return map[string]any{
			"type":                 "object",
			"additionalProperties": ctyTypeToSchema(name+"_val", elem),
			"x-ui-widget":          "key-value",
		}
	case t.IsObjectType():
		attrs := t.AttributeTypes()
		attrNames := make([]string, 0, len(attrs))
		for k := range attrs {
			attrNames = append(attrNames, k)
		}
		sort.Strings(attrNames)
		subProps := make(map[string]any, len(attrNames))
		for i, k := range attrNames {
			sub := ctyTypeToSchema(k, attrs[k])
			sub["title"] = HumanizeIdentifier(k)
			sub["x-ui-order"] = (i + 1) * 10
			if k == "cpu" {
				sub["x-ui-unit"] = "MHz"
				sub["x-ui-presets"] = []int{250, 500, 1000, 2000, 4000}
			} else if k == "memory" || k == "memory_mb" {
				sub["x-ui-unit"] = "MiB"
				sub["x-ui-presets"] = []int{256, 512, 1024, 2048, 4096}
			}
			subProps[k] = sub
		}
		return map[string]any{
			"type":       "object",
			"properties": subProps,
		}
	}
	return map[string]any{"type": "string", "x-ui-widget": "text"}
}

func inferStringWidget(lowerName string) string {
	switch {
	case lowerName == "namespace" || strings.HasSuffix(lowerName, "_namespace"):
		return "namespace-select"
	case lowerName == "node_pool" || strings.HasSuffix(lowerName, "_node_pool"):
		return "node-pool-select"
	case lowerName == "region" || strings.HasSuffix(lowerName, "_region"):
		return "region-select"
	case strings.Contains(lowerName, "config") || strings.Contains(lowerName, "script") || strings.Contains(lowerName, "hba"):
		return "code"
	default:
		return "text"
	}
}

func inferTabAndGroup(name string, prop map[string]any) (string, string) {
	lower := strings.ToLower(name)
	switch {
	case secretVarName.MatchString(lower):
		return "secrets", "Credentials & Secrets"
	case strings.Contains(lower, "cpu") || strings.Contains(lower, "memory") ||
		strings.Contains(lower, "resource") || strings.Contains(lower, "count") ||
		strings.Contains(lower, "instance") || strings.Contains(lower, "replica") ||
		strings.Contains(lower, "scale"):
		return "resources", "Capacity & Scaling"
	case strings.Contains(lower, "port") || strings.Contains(lower, "network") ||
		strings.Contains(lower, "mtls") || strings.Contains(lower, "tls") ||
		strings.Contains(lower, "service") || strings.Contains(lower, "dns") ||
		strings.Contains(lower, "caller") || strings.Contains(lower, "ingress") ||
		strings.Contains(lower, "host"):
		return "network", "Networking & Security"
	case strings.Contains(lower, "volume") || strings.Contains(lower, "storage") ||
		strings.Contains(lower, "disk") || strings.Contains(lower, "backup") ||
		strings.Contains(lower, "data_dir"):
		return "storage", "Storage & Backups"
	case lower == "datacenters" || lower == "region" || lower == "namespace" || lower == "node_pool":
		return "general", "Placement & Topology"
	default:
		return "general", "General Configuration"
	}
}

func mergeSchemaOverlay(base, overlay map[string]any) {
	for k, v := range overlay {
		if k == "properties" {
			baseProps, _ := base["properties"].(map[string]any)
			if baseProps == nil {
				baseProps = map[string]any{}
				base["properties"] = baseProps
			}
			if overProps, ok := v.(map[string]any); ok {
				for pk, pv := range overProps {
					overPropMap, ok := pv.(map[string]any)
					if !ok {
						baseProps[pk] = pv
						continue
					}
					if existing, ok := baseProps[pk].(map[string]any); ok {
						mergePropertyMap(existing, overPropMap)
					} else {
						cloned := maps.Clone(overPropMap)
						if _, hasTitle := cloned["title"]; !hasTitle {
							cloned["title"] = HumanizeIdentifier(pk)
						}
						if _, hasTab := cloned["x-ui-tab"]; !hasTab {
							tab, grp := inferTabAndGroup(pk, cloned)
							cloned["x-ui-tab"] = tab
							if _, hasGrp := cloned["x-ui-group"]; !hasGrp {
								cloned["x-ui-group"] = grp
							}
						}
						if _, hasWidget := cloned["x-ui-widget"]; !hasWidget {
							if enumSlice, ok := cloned["enum"].([]any); ok && len(enumSlice) > 0 {
								if len(enumSlice) <= 3 {
									cloned["x-ui-widget"] = "segmented"
								} else {
									cloned["x-ui-widget"] = "select"
								}
							}
						}
						baseProps[pk] = cloned
					}
				}
			}
			continue
		}
		if k == "required" {
			var merged []string
			if existing, ok := base["required"].([]string); ok {
				merged = append(merged, existing...)
			}
			if overReq, ok := v.([]any); ok {
				for _, item := range overReq {
					if s, ok := item.(string); ok && !slices.Contains(merged, s) {
						merged = append(merged, s)
					}
				}
			}
			if len(merged) > 0 {
				base["required"] = merged
			}
			continue
		}
		base[k] = v
	}
}

func mergePropertyMap(dst, src map[string]any) {
	for k, v := range src {
		if k == "properties" {
			dstSub, _ := dst["properties"].(map[string]any)
			if dstSub == nil {
				dstSub = map[string]any{}
				dst["properties"] = dstSub
			}
			if srcSub, ok := v.(map[string]any); ok {
				for sk, sv := range srcSub {
					if svMap, ok := sv.(map[string]any); ok {
						if existingSub, ok := dstSub[sk].(map[string]any); ok {
							mergePropertyMap(existingSub, svMap)
						} else {
							dstSub[sk] = maps.Clone(svMap)
						}
					} else {
						dstSub[sk] = sv
					}
				}
			}
			continue
		}
		dst[k] = v
	}
	// If enum is provided without an explicit widget, choose segmented (<= 3) or select (> 3).
	if _, hasWidget := dst["x-ui-widget"]; !hasWidget || dst["x-ui-widget"] == "text" || dst["x-ui-widget"] == "number" {
		if enumSlice, ok := dst["enum"].([]any); ok && len(enumSlice) > 0 {
			if len(enumSlice) <= 3 {
				dst["x-ui-widget"] = "segmented"
			} else {
				dst["x-ui-widget"] = "select"
			}
		}
	}
}

var defaultCatalogTabs = []map[string]any{
	{"id": "general", "label": "General", "icon": "settings", "order": 10},
	{"id": "resources", "label": "Resources & Scaling", "icon": "cpu", "order": 20},
	{"id": "network", "label": "Networking & Security", "icon": "shield-lock", "order": 30},
	{"id": "storage", "label": "Storage & Backups", "icon": "database", "order": 40},
	{"id": "secrets", "label": "Credentials & Secrets", "icon": "key", "order": 50},
	{"id": "dependencies", "label": "Dependencies", "icon": "packages", "order": 60},
}

func ensureUITabs(schema map[string]any) {
	usedTabs := map[string]bool{}
	if props, ok := schema["properties"].(map[string]any); ok {
		for _, raw := range props {
			if pm, ok := raw.(map[string]any); ok {
				tab, _ := pm["x-ui-tab"].(string)
				if tab == "" {
					tab = "general"
					pm["x-ui-tab"] = tab
				}
				usedTabs[tab] = true
			}
		}
	}
	if explicit, ok := schema["x-ui-tabs"].([]any); ok {
		// The tabs the pack declares, then the default tabs its variables
		// use but it leaves out.
		declared := map[string]bool{}
		for _, raw := range explicit {
			if t, ok := raw.(map[string]any); ok {
				id, _ := t["id"].(string)
				declared[id] = true
			}
		}
		for _, t := range defaultCatalogTabs {
			id := t["id"].(string)
			if usedTabs[id] && !declared[id] {
				explicit = append(explicit, maps.Clone(t))
			}
		}
		schema["x-ui-tabs"] = explicit
		return
	}
	if _, hasExplicit := schema["x-ui-tabs"]; hasExplicit {
		return
	}
	var tabs []map[string]any
	for _, t := range defaultCatalogTabs {
		id := t["id"].(string)
		if usedTabs[id] {
			tabs = append(tabs, maps.Clone(t))
		}
	}
	if len(tabs) == 0 {
		tabs = append(tabs, maps.Clone(defaultCatalogTabs[0]))
	}
	schema["x-ui-tabs"] = tabs
}

// HumanizeIdentifier turns a snake_case or camelCase identifier into a
// human-readable label (e.g. "postgres_user" -> "Postgres User", "mtls_mode" -> "mTLS Mode").
func HumanizeIdentifier(s string) string {
	if s == "" {
		return ""
	}
	acronyms := map[string]string{
		"cpu": "CPU", "mb": "MB", "gb": "GB", "mib": "MiB", "gib": "GiB",
		"mtls": "mTLS", "tls": "TLS", "ssl": "SSL", "url": "URL", "uri": "URI",
		"id": "ID", "ip": "IP", "dns": "DNS", "sql": "SQL", "api": "API",
		"acl": "ACL", "csi": "CSI", "az": "AZ", "azs": "AZs", "db": "DB",
		"pg": "PostgreSQL", "hba": "HBA", "jwt": "JWT", "oidc": "OIDC",
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	for i, p := range parts {
		lower := strings.ToLower(p)
		if acr, ok := acronyms[lower]; ok {
			parts[i] = acr
			continue
		}
		runes := []rune(p)
		runes[0] = unicode.ToUpper(runes[0])
		parts[i] = string(runes)
	}
	return strings.Join(parts, " ")
}
