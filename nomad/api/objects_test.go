// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestCustomObject_Field(t *testing.T) {
	obj := &CustomObject{
		Group: "db.example.com", Kind: "PostgresCluster", Name: "billing", Namespace: "prod",
		Labels:     map[string]string{"team": "finance"},
		Generation: 3,
		Spec: map[string]any{
			"version": "16",
			"users":   []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}},
		},
		Status: map[string]any{
			"observedGeneration": float64(2),
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "reason": "Running"},
				map[string]any{"type": "Degraded", "status": "False"},
			},
		},
	}

	cases := map[string]any{
		"spec.version":                                "16",
		"spec.users[1].name":                          "b",
		"spec.users[-1].name":                         "b",
		"spec.users[*].name":                          []any{"a", "b"},
		"status.conditions[?type=='Ready'].status":    []any{"True"},
		`status.conditions[?type=="Degraded"].status`: []any{"False"},
		"metadata.labels.team":                        "finance",
		"metadata.generation":                         float64(3),
		"kind":                                        "db.example.com/PostgresCluster",
		"spec.missing":                                nil,
		"status.conditions[?type=='Gone'].status":     []any(nil),
	}
	for path, want := range cases {
		got, err := obj.Field(path)
		must.NoError(t, err, must.Sprint(path))
		must.Eq(t, want, got, must.Sprint(path))
	}
	for _, bad := range []string{"spec.users[x]", "spec.users[0", "status.conditions[?type]"} {
		_, err := obj.Field(bad)
		must.Error(t, err, must.Sprint(bad))
	}

	must.Eq(t, "True", FormatObjectValue([]any{"True"}))
	must.Eq(t, "a,b", FormatObjectValue([]any{"a", "b"}))
	must.Eq(t, "3", FormatObjectValue(float64(3)))
	must.Eq(t, "", FormatObjectValue(nil))

	must.Eq(t, uint64(2), obj.ObservedGeneration())
	must.Len(t, 2, obj.Conditions())
	must.Eq(t, "Running", obj.Condition(ObjectConditionReady).Reason)
	must.Nil(t, obj.Condition("Missing"))
}

func TestObjectWriteOptions_Query(t *testing.T) {
	var nilOpts *ObjectWriteOptions
	must.Eq(t, "", nilOpts.query())
	must.Eq(t, "", (&ObjectWriteOptions{}).query())
	must.Eq(t, "?cas=0&dry_run=true", (&ObjectWriteOptions{CAS: CASIndex(0), DryRun: true}).query())
	must.Eq(t, "?propagation=background", (&ObjectWriteOptions{Propagation: "background"}).query())
}
