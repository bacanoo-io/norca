// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package objectschema

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestCompileDocument_ValidateValue(t *testing.T) {
	s, err := CompileDocument("pack-variables/redis", `{
	  "type": "object",
	  "required": ["image"],
	  "properties": {
	    "image": {"type": "string", "pattern": "^redis:"},
	    "app_count": {"type": "integer", "minimum": 1, "maximum": 3},
	    "datacenters": {"type": "array", "items": {"type": "string"}, "minItems": 1},
	    "resources": {
	      "type": "object",
	      "properties": {"cpu": {"type": "number", "maximum": 4000}}
	    }
	  }
	}`)
	must.NoError(t, err)

	// Go values validate like their JSON encoding.
	must.NoError(t, s.ValidateValue(map[string]any{
		"image":       "redis:7",
		"app_count":   int64(2),
		"datacenters": []string{"dc1"},
		"resources":   map[string]any{"cpu": 500},
	}))

	err = s.ValidateValue(map[string]any{
		"image":       "nginx",
		"app_count":   5,
		"datacenters": []string{},
		"resources":   map[string]any{"cpu": 9000.5},
	})
	must.ErrorContains(t, err, "at /app_count:")
	must.ErrorContains(t, err, "at /image:")
	must.ErrorContains(t, err, "at /datacenters:")
	must.ErrorContains(t, err, "at /resources/cpu:")

	must.ErrorContains(t, s.ValidateValue(map[string]any{}), "missing property 'image'")

	// Schemas can't load other documents.
	_, err = CompileDocument("x", `{"$ref": "https://example.com/other.json"}`)
	must.ErrorContains(t, err, "self-contained")

	_, err = CompileDocument("x", `[1]`)
	must.ErrorContains(t, err, "not a JSON object")
}
