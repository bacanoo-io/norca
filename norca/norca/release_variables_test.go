// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

const testVariablesSchema = `{
  "type": "object",
  "required": ["image"],
  "properties": {
    "image": {"type": "string", "pattern": "^redis:"},
    "app_count": {"type": "integer", "minimum": 1, "maximum": 3}
  }
}`

func TestRelease_VariablesSchema(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	req := func(values map[string]any, dryRun bool) *structs.ReleaseApplyRequest {
		return &structs.ReleaseApplyRequest{
			Pack:             &structs.ReleasePack{Name: "redis"},
			Resources:        []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("vs-redis")}},
			VariablesSchemas: []*structs.ReleaseVariablesSchema{{Pack: "redis", Schema: testVariablesSchema}},
			VariableValues:   map[string]map[string]any{"redis": values},
			DryRun:           dryRun,
		}
	}

	// Values that violate the schema are rejected, also by plans, with the
	// path of the variable.
	for _, dryRun := range []bool{true, false} {
		_, err := c.apply("default", "vs", req(map[string]any{"image": "nginx", "app_count": 5}, dryRun))
		must.ErrorContains(t, err, `the variables of pack "redis" do not match its variables_schema`)
		must.ErrorContains(t, err, "at /app_count:")
		must.ErrorContains(t, err, "at /image:")
	}
	must.Nil(t, c.get("default", "vs"))

	// Unset variables (null) are absent, so required reports them.
	_, err := c.apply("default", "vs", req(map[string]any{"image": nil}, false))
	must.ErrorContains(t, err, "missing property 'image'")

	// An invalid schema is rejected.
	bad := req(map[string]any{"image": "redis:7"}, false)
	bad.VariablesSchemas[0].Schema = `{"type": 5}`
	_, err = c.apply("default", "vs", bad)
	must.ErrorContains(t, err, `the variables_schema of pack "redis" is invalid`)

	// Valid values: the schema is stored in the revision, the values used
	// for the validation are not.
	resp, err := c.apply("default", "vs", req(map[string]any{"image": "redis:7", "app_count": 2, "password": "s3cret"}, false))
	must.NoError(t, err)
	must.Eq(t, 1, resp.Revision)
	rev := c.revision("default", "vs", 1)
	must.Len(t, 1, rev.Spec.VariablesSchemas)
	must.Eq(t, "redis", rev.Spec.VariablesSchemas[0].Pack)
	must.Eq(t, testVariablesSchema, rev.Spec.VariablesSchemas[0].Schema)
	must.Nil(t, rev.Spec.Values)

	// A rollback keeps the schema of the revision it restores.
	_, err = c.apply("default", "vs", req(map[string]any{"image": "redis:8"}, false))
	must.NoError(t, err)
	resp, err = c.rollback("default", "vs", 1)
	must.NoError(t, err)
	rev = c.revision("default", "vs", resp.Revision)
	must.Len(t, 1, rev.Spec.VariablesSchemas)
	must.Eq(t, testVariablesSchema, rev.Spec.VariablesSchemas[0].Schema)
}
