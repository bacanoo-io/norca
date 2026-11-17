// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package objectschema

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/shoenig/test/must"
)

const pgSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["version"],
      "additionalProperties": false,
      "properties": {
        "version":  {"type": "string"},
        "replicas": {"type": "integer", "minimum": 1, "default": 1},
        "storage":  {"type": "string", "default": "10GiB"},
        "backup": {
          "type": "object",
          "default": {},
          "properties": {
            "schedule":  {"type": "string", "default": "0 2 * * *"},
            "retention": {"$ref": "#/$defs/retention"}
          }
        },
        "users": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "name":  {"type": "string"},
              "admin": {"type": "boolean", "default": false}
            }
          }
        }
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "readyReplicas": {"type": "integer"}
      }
    }
  },
  "$defs": {
    "retention": {"type": "integer", "default": 7}
  }
}`

func doc(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	must.NoError(t, json.Unmarshal([]byte(s), &m))
	return m
}

func TestSchema_Defaults(t *testing.T) {
	s, err := Compile("db.example.com", "PostgresCluster", pgSchema)
	must.NoError(t, err)

	d := doc(t, `{"spec": {"version": "16", "replicas": 3, "users": [{"name": "a"}, {"name": "b", "admin": true}]}}`)
	s.ApplyDefaults(d)
	must.Eq(t, doc(t, `{"spec": {
		"version": "16", "replicas": 3, "storage": "10GiB",
		"backup": {"schedule": "0 2 * * *", "retention": 7},
		"users": [{"name": "a", "admin": false}, {"name": "b", "admin": true}]}}`), d)
	must.NoError(t, s.Validate(d))

	// A default is never filled into an explicitly set value, and a
	// missing status is not created.
	d = doc(t, `{"spec": {"version": "16", "backup": {"schedule": "@daily"}}}`)
	s.ApplyDefaults(d)
	must.Eq(t, "@daily", d["spec"].(map[string]any)["backup"].(map[string]any)["schedule"])
	_, hasStatus := d["status"]
	must.False(t, hasStatus)
}

func TestSchema_Validate(t *testing.T) {
	s, err := Compile("db.example.com", "PostgresCluster", pgSchema)
	must.NoError(t, err)

	err = s.Validate(doc(t, `{"spec": {"replicas": 0, "extra": true}}`))
	must.ErrorContains(t, err, "at /spec: missing property 'version'")
	must.ErrorContains(t, err, "at /spec/replicas: minimum")
	must.ErrorContains(t, err, "extra")

	err = s.Validate(doc(t, `{"spec": {"version": "16"}, "status": {"readyReplicas": "three"}}`))
	must.ErrorContains(t, err, "at /status/readyReplicas")

	// Integral float64 values (from encoding/json) are integers.
	must.NoError(t, s.Validate(map[string]any{"spec": map[string]any{"version": "16", "replicas": float64(2)}}))
}

func TestSchema_CompileErrors(t *testing.T) {
	cases := map[string]string{
		"not an object":   `[1, 2]`,
		"invalid JSON":    `{"type": `,
		"invalid keyword": `{"type": "objekt"}`,
		"remote ref":      `{"properties": {"spec": {"$ref": "https://example.com/schema.json"}}}`,
		"file ref":        `{"properties": {"spec": {"$ref": "file:///etc/passwd"}}}`,
		"bad default":     `{"properties": {"spec": {"properties": {"replicas": {"type": "integer", "default": "one"}}}}}`,
		"bad ref default": `{"properties": {"spec": {"properties": {"r": {"$ref": "#/$defs/r"}}}}, "$defs": {"r": {"type": "integer", "default": 1.5}}}`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Compile("db.example.com", "Bad", schema)
			must.Error(t, err)
		})
	}

	// An older draft can be selected with $schema.
	_, err := Compile("db.example.com", "Old", `{"$schema": "http://json-schema.org/draft-07/schema#", "type": "object"}`)
	must.NoError(t, err)
}

// Defaults that fill in each other through references double the document
// on every level (review finding SRV-10): such a schema is refused, and what
// one filling adds to a document is bounded.
func TestSchema_DefaultsBounded(t *testing.T) {
	// A default that fills in itself.
	_, err := Compile("example.com", "Tree", `{
  "type": "object",
  "properties": {"spec": {"$ref": "#/$defs/n"}},
  "$defs": {"n": {"type": "object", "properties": {
    "a": {"$ref": "#/$defs/n", "default": {}},
    "b": {"$ref": "#/$defs/n", "default": {}}
  }}}
}`)
	must.ErrorContains(t, err, "grow without bound")

	// A chain without a cycle: every level has two properties that default
	// to the next level.
	chain := func(levels int) string {
		var b strings.Builder
		b.WriteString(`{"type": "object", "properties": {"spec": {"$ref": "#/$defs/n0"}}, "$defs": {`)
		for i := 0; i < levels; i++ {
			fmt.Fprintf(&b, `"n%d": {"type": "object", "properties": {"a": {"$ref": "#/$defs/n%d", "default": {}}, "b": {"$ref": "#/$defs/n%d", "default": {}}}}, `, i, i+1, i+1)
		}
		fmt.Fprintf(&b, `"n%d": {"type": "object"}}}`, levels)
		return b.String()
	}
	_, err = Compile("example.com", "Chain", chain(40))
	must.ErrorContains(t, err, "grow without bound")

	// A small one is fine.
	s, err := Compile("example.com", "Chain", chain(3))
	must.NoError(t, err)
	d := doc(t, `{"spec": {}}`)
	must.NoError(t, s.ApplyDefaults(d))
	must.MapLen(t, 2, d["spec"].(map[string]any)["a"].(map[string]any)["b"].(map[string]any))

	// A schema whose defaults are small, filled into many items of the
	// document: the filling stops at the limit.
	s, err = Compile("example.com", "List", `{
  "type": "object",
  "properties": {"spec": {"type": "object", "properties": {"items": {"type": "array", "items": {
    "type": "object", "properties": {"text": {"type": "string", "default": "`+strings.Repeat("x", 1000)+`"}}
  }}}}}
}`)
	must.NoError(t, err)
	items := make([]any, 1000)
	for i := range items {
		items[i] = map[string]any{}
	}
	d = map[string]any{"spec": map[string]any{"items": items}}
	must.ErrorIs(t, s.ApplyDefaults(d), ErrDefaultsTooLarge)
	filled := 0
	for _, item := range items {
		filled += len(item.(map[string]any))
	}
	must.Less(t, 300, filled)

	d = map[string]any{"spec": map[string]any{"items": []any{map[string]any{}}}}
	must.NoError(t, s.ApplyDefaults(d))
}
