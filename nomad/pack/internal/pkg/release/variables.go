// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package release

import (
	"fmt"

	"github.com/hashicorp/nomad/nomad/objectschema"
)

// ValidateVariables validates the effective variable values of a pack
// (pack is its path in the dependency tree, e.g. "app.redis") against its
// variables_schema, with the JSON Schema validator the servers use for
// object kinds and releases. Variables without a value are left out, so
// that "required" reports unset variables. Errors name the failing variable
// path, such as "at /resources/cpu: maximum: got 9000, want 4000".
func ValidateVariables(pack, schema string, values map[string]any) error {
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
