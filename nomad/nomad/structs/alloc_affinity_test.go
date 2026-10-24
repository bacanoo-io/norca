// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestAllocAffinity_Validate(t *testing.T) {
	ci.Parallel(t)

	valid := func() *AllocAffinity {
		return &AllocAffinity{
			Selector: map[string]string{"app": "api"},
			Scope:    "${node.unique.id}",
			Weight:   50,
		}
	}

	cases := []struct {
		name    string
		mod     func(a *AllocAffinity)
		jobType string
		err     string
	}{
		{name: "valid preferred", mod: func(*AllocAffinity) {}},
		{name: "valid required", mod: func(a *AllocAffinity) { a.Weight = 0; a.Required = true }},
		{name: "valid negative weight", mod: func(a *AllocAffinity) { a.Weight = -100 }},
		{name: "valid attr scope", mod: func(a *AllocAffinity) { a.Scope = "${attr.platform.aws.placement.availability-zone}" }},
		{name: "valid meta scope", mod: func(a *AllocAffinity) { a.Scope = "${meta.rack}" }},
		{name: "valid namespace", mod: func(a *AllocAffinity) { a.Namespace = "other" }},
		{name: "empty selector", mod: func(a *AllocAffinity) { a.Selector = nil }, err: "Missing selector"},
		{name: "empty selector key", mod: func(a *AllocAffinity) { a.Selector[""] = "x" }, err: "Selector keys"},
		{name: "literal scope", mod: func(a *AllocAffinity) { a.Scope = "rack" }, err: "Invalid scope"},
		{name: "unknown scope", mod: func(a *AllocAffinity) { a.Scope = "${node.foo}" }, err: "Invalid scope"},
		{name: "empty meta scope", mod: func(a *AllocAffinity) { a.Scope = "${meta.}" }, err: "Invalid scope"},
		{name: "zero weight", mod: func(a *AllocAffinity) { a.Weight = 0 }, err: "Weight must be a non-zero"},
		{name: "weight too large", mod: func(a *AllocAffinity) { a.Weight = 101 }, err: "Weight must be a non-zero"},
		{name: "weight on required", mod: func(a *AllocAffinity) { a.Required = true }, err: "Weight must not be set"},
		{name: "wildcard namespace", mod: func(a *AllocAffinity) { a.Namespace = "*" }, err: "Invalid namespace"},
		{name: "system preferred", mod: func(*AllocAffinity) {}, jobType: JobTypeSystem, err: "only have required"},
		{name: "system required", mod: func(a *AllocAffinity) { a.Weight = 0; a.Required = true }, jobType: JobTypeSystem},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := valid()
			tc.mod(a)
			jobType := tc.jobType
			if jobType == "" {
				jobType = JobTypeService
			}
			err := a.Validate(AllocAffinityKindColocate, jobType)
			if tc.err == "" {
				must.NoError(t, err)
			} else {
				must.ErrorContains(t, err, tc.err)
			}
		})
	}
}

func TestAllocAffinity_TaskGroupValidate(t *testing.T) {
	ci.Parallel(t)

	job := testJob()
	tg := job.TaskGroups[0]
	for i := 0; i < MaxAllocAffinitiesPerGroup; i++ {
		tg.Colocate = append(tg.Colocate, &AllocAffinity{
			Selector: map[string]string{"app": "api"}, Scope: AllocAffinityDefaultScope, Weight: 10})
	}
	must.NoError(t, tg.Validate(job))

	tg.Separate = append(tg.Separate, &AllocAffinity{Selector: map[string]string{"app": "x"}})
	err := tg.Validate(job)
	must.ErrorContains(t, err, "at most 8 colocate and separate rules")
	must.ErrorContains(t, err, "Separate 1 validation failed")
}

func TestAllocAffinity_Matches(t *testing.T) {
	ci.Parallel(t)

	a := &AllocAffinity{Selector: map[string]string{"app": "api", "tier": "web"}}
	must.True(t, a.Matches(map[string]string{"app": "api"}, map[string]string{"tier": "web"}))
	must.True(t, a.Matches(map[string]string{"app": "api", "tier": "web", "x": "y"}, nil))
	// task group meta overrides job meta
	must.False(t, a.Matches(map[string]string{"app": "api", "tier": "web"}, map[string]string{"tier": "db"}))
	must.True(t, a.Matches(map[string]string{"app": "cache", "tier": "web"}, map[string]string{"app": "api"}))
	must.False(t, a.Matches(map[string]string{"app": "api"}, nil))
	must.False(t, (&AllocAffinity{}).Matches(map[string]string{"app": "api"}, nil))
}

func TestAllocAffinity_CopyEqualCanonicalize(t *testing.T) {
	ci.Parallel(t)

	a := &AllocAffinity{Selector: map[string]string{"app": "api"}, Weight: 20}
	a.Canonicalize()
	must.Eq(t, AllocAffinityDefaultScope, a.Scope)

	b := a.Copy()
	must.True(t, a.Equal(b))
	b.Selector["app"] = "other"
	must.Eq(t, "api", a.Selector["app"])
	must.False(t, a.Equal(b))

	tg := &TaskGroup{Colocate: []*AllocAffinity{a}, Separate: []*AllocAffinity{a.Copy()}}
	ntg := tg.Copy()
	ntg.Colocate[0].Weight = 99
	ntg.Separate[0].Selector["k"] = "v"
	must.Eq(t, 20, a.Weight)
	must.MapNotContainsKey(t, tg.Separate[0].Selector, "k")

	must.Eq(t, "{other/app=api,b=c} by ${meta.rack}", (&AllocAffinity{
		Namespace: "other", Selector: map[string]string{"b": "c", "app": "api"}, Scope: "${meta.rack}",
	}).String())
}

func TestAllocAffinity_JobNamespaces(t *testing.T) {
	ci.Parallel(t)

	job := testJob()
	must.False(t, job.UsesAllocAffinities())
	job.TaskGroups[0].Colocate = []*AllocAffinity{
		{Selector: map[string]string{"a": "b"}, Namespace: "zeta"},
		{Selector: map[string]string{"a": "b"}},
	}
	job.TaskGroups[0].Separate = []*AllocAffinity{
		{Selector: map[string]string{"a": "b"}, Namespace: "alpha"},
		{Selector: map[string]string{"a": "b"}, Namespace: "zeta"},
		{Selector: map[string]string{"a": "b"}, Namespace: job.Namespace},
	}
	must.True(t, job.UsesAllocAffinities())
	must.Eq(t, []string{"alpha", "zeta"}, job.AllocAffinityNamespaces())
}

func TestAllocAffinity_TaskGroupDiff(t *testing.T) {
	ci.Parallel(t)

	old := &TaskGroup{Name: "web", Colocate: []*AllocAffinity{{
		Selector: map[string]string{"app": "api"}, Scope: "${node.unique.id}", Weight: 50,
	}}}
	new := &TaskGroup{Name: "web", Colocate: []*AllocAffinity{{
		Selector: map[string]string{"app": "api"}, Scope: "${node.unique.id}", Weight: 50,
	}}, Separate: []*AllocAffinity{{
		Selector: map[string]string{"app": "cache"}, Scope: "${meta.zone}", Required: true,
	}}}

	diff, err := old.Diff(new, false)
	must.NoError(t, err)
	must.Eq(t, DiffTypeEdited, diff.Type)
	must.Len(t, 1, diff.Objects)
	obj := diff.Objects[0]
	must.Eq(t, "Separate", obj.Name)
	must.Eq(t, DiffTypeAdded, obj.Type)

	fields := make(map[string]string)
	for _, f := range obj.Fields {
		fields[f.Name] = f.New
	}
	must.Eq(t, map[string]string{
		"Required":      "true",
		"Scope":         "${meta.zone}",
		"Selector[app]": "cache",
		"Weight":        "0",
	}, fields)
}
