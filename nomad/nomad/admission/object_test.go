// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"context"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func objectValue(t *testing.T, spec map[string]any) any {
	t.Helper()
	v, err := ObjectValue(map[string]any{"metadata": map[string]any{"name": "billing"}, "spec": spec})
	must.NoError(t, err)
	return v
}

func TestObjectRules_OldSelf(t *testing.T) {
	ci.Parallel(t)

	engine, err := NewObjectRuleEngine()
	must.NoError(t, err)

	kind := &structs.ObjectKind{
		Group: "db.example.com", Kind: "PostgresCluster", ModifyIndex: 10,
		Rules: []*structs.ObjectKindRule{
			{Expression: "self.spec.replicas >= 1 && self.spec.replicas <= 7", Message: "The number of replicas must be between 1 and 7."},
			{Expression: "!has(oldSelf) || quantity(self.spec.storage) >= quantity(oldSelf.spec.storage)", Message: "Storage cannot be reduced."},
			{Expression: "request.operation != 'update' || has(oldSelf)"},
		},
	}
	must.NoError(t, engine.CompileRules(kind))

	ctx := context.Background()
	create := map[string]any{"operation": "create"}
	update := map[string]any{"operation": "update"}

	// Create: oldSelf is null.
	v := engine.Evaluate(ctx, kind, objectValue(t, map[string]any{"replicas": 3, "storage": "100GiB"}), nil, create)
	must.SliceEmpty(t, v)

	old := objectValue(t, map[string]any{"replicas": 3, "storage": "100GiB"})

	// Increasing the storage is allowed, reducing it is not.
	v = engine.Evaluate(ctx, kind, objectValue(t, map[string]any{"replicas": 3, "storage": "200Gi"}), old, update)
	must.SliceEmpty(t, v)
	v = engine.Evaluate(ctx, kind, objectValue(t, map[string]any{"replicas": 3, "storage": "50GiB"}), old, update)
	must.Len(t, 1, v)
	must.Eq(t, "Storage cannot be reduced.", v[0].Message)
	must.Eq(t, 1, v[0].Rule)

	v = engine.Evaluate(ctx, kind, objectValue(t, map[string]any{"replicas": 9.0, "storage": "100GiB"}), old, update)
	must.Len(t, 1, v)
	must.Eq(t, "The number of replicas must be between 1 and 7.", v[0].Message)

	// A rule that fails to evaluate is violated.
	v = engine.Evaluate(ctx, kind, objectValue(t, map[string]any{"replicas": 3, "storage": "lots"}), old, update)
	must.Len(t, 1, v)
	must.StrContains(t, v[0].Message, "failed to evaluate")
}

func TestObjectRules_CompileErrors(t *testing.T) {
	ci.Parallel(t)

	engine, err := NewObjectRuleEngine()
	must.NoError(t, err)

	err = engine.CompileRules(&structs.ObjectKind{
		Group: "db.example.com", Kind: "X",
		Rules: []*structs.ObjectKindRule{
			{Expression: "self.spec.replicas >="},
			{Expression: "'not a bool'"},
			{Expression: "has(self)"},
			{Expression: "unknown.x == 1"},
		},
	})
	must.ErrorContains(t, err, "rule 0")
	must.ErrorContains(t, err, "rule 1")
	must.ErrorContains(t, err, "rule 2")
	must.ErrorContains(t, err, "rule 3")
}

func TestParseQuantity(t *testing.T) {
	ci.Parallel(t)

	cases := map[string]float64{
		"100GiB": 100 * (1 << 30),
		"1.5Gi":  1.5 * (1 << 30),
		"2k":     2000,
		"2K":     2000,
		"10":     10,
		"10B":    10,
		"500m":   0.5,
		"1T":     1e12,
		" 3Mi ":  3 * (1 << 20),
		"-1Ki":   -1024,
		"1e3":    1000,
	}
	for in, want := range cases {
		got, err := ParseQuantity(in)
		must.NoError(t, err, must.Sprint(in))
		must.Eq(t, want, got, must.Sprint(in))
	}
	for _, in := range []string{"", "Gi", "1.2.3", "10Xi", "5mB", "abc"} {
		_, err := ParseQuantity(in)
		must.Error(t, err, must.Sprint(in))
	}
}
