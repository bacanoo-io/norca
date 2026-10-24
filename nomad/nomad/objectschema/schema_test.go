// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package objectschema

import (
	"encoding/json"
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
