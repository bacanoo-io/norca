// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import "github.com/zclconf/go-cty/cty"

// Facts are the facts of an install: the rigger.* of an evaluation
// (design 4.2).
type Facts struct {
	Release       string
	Namespace     string
	Region        string
	ModuleName    string
	ModuleVersion string
}

// value returns the object rigger.* of the facts.
func (f Facts) value() cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"release":   cty.StringVal(f.Release),
		"namespace": cty.StringVal(f.Namespace),
		"region":    cty.StringVal(f.Region),
		"module": cty.ObjectVal(map[string]cty.Value{
			"name":    cty.StringVal(f.ModuleName),
			"version": cty.StringVal(f.ModuleVersion),
		}),
	})
}
