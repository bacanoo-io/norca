// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/hashicorp/nomad/nomad/objectschema"
	"github.com/hashicorp/nomad/nomad/structs"
)

// validateReleaseVariables validates the effective variable values of a pack
// and its dependencies against their variables schemas (variables_schema in
// metadata.hcl), with the JSON Schema validator of object kinds. The client
// validates them before it renders the pack; the servers check again, so
// that releases applied through the API keep to the schemas too. The values
// of a pack without an entry in values validate as an empty object.
func validateReleaseVariables(schemas []*structs.ReleaseVariablesSchema, values map[string]map[string]any) error {
	size := 0
	var packs []string
	for _, s := range schemas {
		if s == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "empty variables schema")
		}
		if slices.Contains(packs, s.Pack) {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "pack %q has more than one variables schema", s.Pack)
		}
		packs = append(packs, s.Pack)
		size += len(s.Schema)
	}
	if size > structs.MaxReleaseVariablesSchemaSize {
		return structs.NewErrRPCCodedf(http.StatusRequestEntityTooLarge,
			"the variables schemas are %d bytes, at most %d are allowed", size, structs.MaxReleaseVariablesSchemaSize)
	}
	for _, s := range schemas {
		if err := validatePackVariables(s.Pack, s.Schema, values[s.Pack]); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
		}
	}
	return nil
}

// validatePackVariables validates the variable values of one pack against
// its variables schema. Variables without a value (null) are left out, so
// that "required" reports unset variables.
func validatePackVariables(pack, schema string, values map[string]any) error {
	compiled, err := objectschema.CompileDocument("pack-variables/"+pack, schema)
	if err != nil {
		return fmt.Errorf("the variables_schema of pack %q is invalid: %v", pack, err)
	}
	doc := make(map[string]any, len(values))
	for k, v := range values {
		if v != nil {
			doc[k] = v
		}
	}
	if err := compiled.ValidateValue(doc); err != nil {
		return fmt.Errorf("the variables of pack %q do not match its variables_schema: %v", pack, err)
	}
	return nil
}
