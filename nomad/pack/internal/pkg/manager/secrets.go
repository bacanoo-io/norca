// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package manager

import (
	"sort"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/pack/internal/pkg/variable/parser"
	"github.com/zclconf/go-cty/cty"
)

// extractSecrets takes the values of the root pack variables that have a
// secret block out of the render: each becomes an item of the Variable the
// block names, applied by the servers but never stored in the release, and
// the templates see the default of the variable (Norca extension).
func (pm *PackManager) extractSecrets(pv *parser.ParsedVariables) {
	pm.secrets, pm.secretVars = nil, nil
	root := pm.loadedPack.VariablesPath()
	byPath := map[string]*api.ReleaseVariableSecret{}
	for id, v := range pv.GetVars()[root] {
		path, _ := v.UI["x-ui-secret-path"].(string)
		item, _ := v.UI["x-ui-secret-item"].(string)
		if path == "" || item == "" {
			continue
		}
		pm.secretVars = append(pm.secretVars, string(id))
		val := v.Value
		if val == cty.NilVal || val.IsNull() || !val.IsKnown() || val.Type() != cty.String ||
			val.AsString() == "" || (v.Default != cty.NilVal && val.RawEquals(v.Default)) {
			continue
		}
		s, ok := byPath[path]
		if !ok {
			s = &api.ReleaseVariableSecret{Path: path, Items: map[string]string{}}
			byPath[path] = s
		}
		s.Items[item] = val.AsString()
		if v.Default != cty.NilVal {
			v.Value = v.Default
		} else {
			v.Value = cty.StringVal("")
		}
		// The values sent for validation by the servers leave it out.
		delete(pm.schemaValues[root.String()], string(id))
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		pm.secrets = append(pm.secrets, byPath[p])
	}
	sort.Strings(pm.secretVars)
}

// Secrets returns the Variable secrets of the variables with a secret
// block that have a value, set by ProcessTemplates.
func (pm *PackManager) Secrets() []*api.ReleaseVariableSecret {
	return pm.secrets
}

// SecretVariables returns the names of the root pack variables with a
// secret block: their values are not release values.
func (pm *PackManager) SecretVariables() []string {
	return pm.secretVars
}

// MergeSecrets adds the items of extra to secrets; the items of secrets
// win, so -var-secret overrides a variable with a secret block.
func MergeSecrets(secrets, extra []*api.ReleaseVariableSecret) []*api.ReleaseVariableSecret {
	out := secrets
	for _, e := range extra {
		var target *api.ReleaseVariableSecret
		for _, s := range out {
			if s.Path == e.Path && s.Namespace == e.Namespace {
				target = s
				break
			}
		}
		if target == nil {
			out = append(out, &api.ReleaseVariableSecret{Namespace: e.Namespace, Path: e.Path, Items: map[string]string{}})
			target = out[len(out)-1]
		}
		if target.Items == nil {
			target.Items = map[string]string{}
		}
		for k, v := range e.Items {
			if _, ok := target.Items[k]; !ok {
				target.Items[k] = v
			}
		}
	}
	return out
}
