// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package manager

import (
	"fmt"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/pack/internal/pkg/errors"
	"github.com/hashicorp/nomad/pack/internal/pkg/release"
	"github.com/hashicorp/nomad/pack/internal/pkg/variable/parser"
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
		if p.Metadata != nil && p.Metadata.VariablesSchema != "" {
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
			if err := release.ValidateVariables(path, p.Metadata.VariablesSchema, values); err != nil {
				wErrs = append(wErrs, schemaError(path, err))
			}
			pm.schemas = append(pm.schemas, &api.ReleaseVariablesSchema{Pack: path, Schema: p.Metadata.VariablesSchema})
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
