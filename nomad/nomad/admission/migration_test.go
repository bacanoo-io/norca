// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"context"
	"testing"

	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestMigrationEngine(t *testing.T) {
	e, err := NewMigrationEngine()
	must.NoError(t, err)

	obj := map[string]any{
		"kind":     "db.example.com/PostgresCluster",
		"metadata": map[string]any{"name": "a", "kindVersion": 1},
		"spec":     map[string]any{"port": 80, "name": "web", "replicas": 2},
		"status":   map[string]any{"ready": true, "old": "x"},
	}
	self, err := ObjectValue(obj)
	must.NoError(t, err)

	p, err := e.Prepare(&structs.ObjectKindMigration{
		Spec:   `self.spec.omit(["port"]).merge({"ports": [self.spec.port], "from": fromVersion, "to": toVersion})`,
		Status: `self.status.omit(["old"])`,
	}, 3)
	must.NoError(t, err)
	spec, status, err := p.Migrate(context.Background(), self, 1, obj["spec"].(map[string]any), obj["status"].(map[string]any))
	must.NoError(t, err)
	must.Eq(t, map[string]any{"name": "web", "replicas": float64(2), "ports": []any{float64(80)}, "from": float64(1), "to": float64(3)}, spec)
	must.Eq(t, map[string]any{"ready": true}, status)

	// merge replaces existing keys; parts without expressions are kept.
	p, err = e.Prepare(&structs.ObjectKindMigration{Spec: `self.spec.merge({"replicas": self.spec.replicas * 2})`}, 2)
	must.NoError(t, err)
	spec, status, err = p.Migrate(context.Background(), self, 1, obj["spec"].(map[string]any), obj["status"].(map[string]any))
	must.NoError(t, err)
	must.Eq[any](t, float64(4), spec["replicas"])
	must.Eq(t, obj["status"], any(status))

	// Errors: not a map, does not compile, fails to evaluate.
	_, err = e.Prepare(&structs.ObjectKindMigration{Spec: `1 + 1`}, 2)
	must.ErrorContains(t, err, "must evaluate to a map")
	_, err = e.Prepare(&structs.ObjectKindMigration{Spec: `self.spec.(`}, 2)
	must.ErrorContains(t, err, "spec:")
	must.Error(t, e.Compile(&structs.ObjectKindMigration{Status: `[1]`}))
	must.NoError(t, e.Compile(nil))

	p, err = e.Prepare(&structs.ObjectKindMigration{Spec: `self.spec.missing.merge({})`}, 2)
	must.NoError(t, err)
	_, _, err = p.Migrate(context.Background(), self, 1, nil, nil)
	must.ErrorContains(t, err, "spec migration failed")

	p, err = e.Prepare(&structs.ObjectKindMigration{Spec: `true ? self.spec : {}`}, 2)
	must.NoError(t, err)
	spec, _, err = p.Migrate(context.Background(), self, 1, nil, nil)
	must.NoError(t, err)
	must.Eq[any](t, "web", spec["name"])
}

func TestMigrationEngine_ForKind(t *testing.T) {
	e, err := NewMigrationEngine()
	must.NoError(t, err)
	kind := &structs.ObjectKind{Group: "g.example.com", Kind: "K", Version: 2, ModifyIndex: 10,
		Migration: &structs.ObjectKindMigration{Spec: `{"a": 1}`}}
	p1, err := e.ForKind(kind)
	must.NoError(t, err)
	p2, err := e.ForKind(kind)
	must.NoError(t, err)
	must.True(t, p1 == p2, must.Sprint("cached per modify index"))
	e.Forget(kind.KindRef())
	p3, err := e.ForKind(kind)
	must.NoError(t, err)
	must.False(t, p1 == p3)
	must.Eq(t, uint64(2), p3.toVersion)
}
