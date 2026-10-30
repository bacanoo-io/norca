// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package manager

import (
	"encoding/json"
	"fmt"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/pack/internal/pkg/errors"
	"github.com/hashicorp/nomad/pack/internal/pkg/release"
	"github.com/hashicorp/nomad/pack/internal/pkg/variable/parser"
	"github.com/hashicorp/nomad/pack/internal/pkg/variable/rules"
	"github.com/hashicorp/nomad/pack/sdk/pack"
	"github.com/hashicorp/nomad/pack/sdk/pack/variables"
)

// validateVariables validates the effective variable values of every pack
// of the tree that has a variables_schema in its metadata (Norca extension)
// and keeps the schemas and values for the release request, so that the
// servers can validate them again.
func (pm *PackManager) validateVariables(pv *parser.ParsedVariables) []*errors.WrappedUIContext {
	pm.schemas, pm.schemaValues = nil, nil
	all := pv.GetVars()

	var wErrs []*errors.WrappedUIContext
	var walk func(p *pack.Pack)
	walk = func(p *pack.Pack) {
		if err := evaluateRules(p, all[p.VariablesPath()]); err != nil {
			wErrs = append(wErrs, schemaError(p.VariablesPath().String(), err))
		}
		doc := ""
		if p.Metadata != nil {
			doc = p.Metadata.VariablesSchema
		}
		if doc == "" {
			doc = constraintsSchema(p, all[p.VariablesPath()])
		}
		if doc != "" {
			path := p.VariablesPath().String()
			values := map[string]any{}
			for id, v := range all[p.VariablesPath()] {
				val, err := variables.ConvertCtyToInterface(v.Value)
				if err != nil {
					wErrs = append(wErrs, schemaError(path, fmt.Errorf("variable %q: %v", id, err)))
					continue
				}
				values[string(id)] = val
			}
			if err := release.ValidateVariables(path, doc, values); err != nil {
				wErrs = append(wErrs, schemaError(path, err))
			}
			pm.schemas = append(pm.schemas, &api.ReleaseVariablesSchema{Pack: path, Schema: doc})
			if pm.schemaValues == nil {
				pm.schemaValues = map[string]map[string]any{}
			}
			pm.schemaValues[path] = values
		}
		for _, d := range p.Dependencies() {
			walk(d)
		}
	}
	walk(pm.loadedPack)
	return wErrs
}

// evaluateRules evaluates the rule blocks of the variables file of a pack.
func evaluateRules(p *pack.Pack, vars map[variables.ID]*variables.Variable) error {
	f := p.RootVariableFile
	if f == nil || len(f.Content) == 0 {
		return nil
	}
	parsed, diags := rules.Parse(f.Path, f.Content)
	if diags.HasErrors() {
		return diags
	}
	return rules.Evaluate(parsed, vars)
}

// constraintsSchema is the variables schema of a pack without a
// variables_schema whose variables have validation blocks or ui options:
// its synthesized form schema, without "required" (the parser reports
// unset variables). It is "" for a pack without constraints.
func constraintsSchema(p *pack.Pack, vars map[variables.ID]*variables.Variable) string {
	constrained := false
	for _, v := range vars {
		if v != nil && v.HasConstraints() {
			constrained = true
			break
		}
	}
	if !constrained {
		return ""
	}
	name := p.Name()
	if p.Metadata != nil && p.Metadata.Pack != nil && p.Metadata.Pack.Name != "" {
		name = p.Metadata.Pack.Name
	}
	var tabs []any
	if p.Metadata != nil {
		tabs = p.Metadata.UI.FormTabs()
	}
	id := p.VariablesPath()
	doc := variables.SynthesizeUIFormSchema(id, name, map[pack.ID]map[variables.ID]*variables.Variable{id: vars}, tabs, nil)
	delete(doc, "required")
	b, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	return string(b)
}

func schemaError(packPath string, err error) *errors.WrappedUIContext {
	ctx := errors.NewUIErrorContext()
	ctx.Add(errors.UIContextPrefixPackName, packPath)
	return &errors.WrappedUIContext{Err: err, Subject: "invalid variable values", Context: ctx}
}

// VariablesSchemas returns the variables schemas of the packs and the
// effective values validated against them, by pack path. They are set once
// the templates are processed.
func (pm *PackManager) VariablesSchemas() ([]*api.ReleaseVariablesSchema, map[string]map[string]any) {
	return pm.schemas, pm.schemaValues
}

// UIFormSchema synthesizes the unified Draft 2020-12 JSON Schema annotated
// with x-ui-* directives (tabs, groups, widgets, defaults, required) for the
// loaded pack and its dependencies.
func (pm *PackManager) UIFormSchema() (map[string]any, error) {
	pv, wErr := pm.ProcessVariableFiles()
	if wErr != nil && len(wErr) > 0 {
		return nil, wErr[0].Err
	}
	rawSchemas := map[string]string{}
	var walk func(p *pack.Pack)
	walk = func(p *pack.Pack) {
		if p.Metadata != nil && p.Metadata.VariablesSchema != "" {
			rawSchemas[p.VariablesPath().String()] = p.Metadata.VariablesSchema
		}
		for _, d := range p.Dependencies() {
			walk(d)
		}
	}
	walk(pm.loadedPack)
	title := pm.PackName()
	if pm.loadedPack != nil && pm.loadedPack.Metadata != nil && pm.loadedPack.Metadata.Pack != nil && pm.loadedPack.Metadata.Pack.Name != "" {
		title = pm.loadedPack.Metadata.Pack.Name
	}
	var tabs []any
	if pm.loadedPack.Metadata != nil {
		tabs = pm.loadedPack.Metadata.UI.FormTabs()
	}
	return variables.SynthesizeUIFormSchema(pm.loadedPack.VariablesPath(), title, pv.GetVars(), tabs, rawSchemas), nil
}

// SynthesizedVariablesSchemas returns the ReleaseVariablesSchema slice for a
// release revision: the pack's explicit variables_schema definitions when
// present in metadata.hcl, or a synthesized UIFormSchema from variables.hcl
// when the pack has no explicit variables_schema.
func (pm *PackManager) SynthesizedVariablesSchemas() []*api.ReleaseVariablesSchema {
	if len(pm.schemas) > 0 {
		return pm.schemas
	}
	uiSchema, err := pm.UIFormSchema()
	if err != nil || uiSchema == nil {
		return nil
	}
	b, err := json.Marshal(uiSchema)
	if err != nil {
		return nil
	}
	rootPath := pm.PackName()
	if pm.loadedPack != nil {
		rootPath = pm.loadedPack.VariablesPath().String()
	}
	return []*api.ReleaseVariablesSchema{{Pack: rootPath, Schema: string(b)}}
}
