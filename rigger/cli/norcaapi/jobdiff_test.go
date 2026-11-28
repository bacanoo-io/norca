// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"fmt"
	"slices"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca/rigger/host"
)

// The release and the kind of the changes of the tests.
const (
	diffRelease = "web"
	diffKind    = "job"
)

func TestJobDiffLines(t *testing.T) {
	t.Parallel()

	edited := &api.JobDiff{
		Type:   diffEdited,
		Fields: []*api.FieldDiff{{Type: diffEdited, Name: "Priority", Old: "50", New: "60"}, {Type: "None", Name: "Type"}},
		TaskGroups: []*api.TaskGroupDiff{
			{
				Type: diffEdited, Name: diffRelease, Fields: []*api.FieldDiff{{Type: diffEdited, Name: "Count", Old: "1", New: "3"}},
				Tasks: []*api.TaskDiff{{
					Type: diffEdited, Name: "app",
					Objects: []*api.ObjectDiff{
						{Name: "Config", Fields: []*api.FieldDiff{{Type: diffEdited, Name: "image", Old: "web:1.4.2", New: "web:1.4.3"}}},
						{Name: "Env", Fields: []*api.FieldDiff{{Type: diffAdded, Name: "DB_PASSWORD", New: "s3cret"}}},
					},
				}},
			},
			{Type: diffAdded, Name: "worker"},
		},
	}

	tests := []struct {
		name string
		diff *api.JobDiff
		want []string
	}{
		{name: "none", diff: nil, want: nil},
		{name: "no changes", diff: &api.JobDiff{Type: "None"}, want: nil},
		{name: "a new job", diff: &api.JobDiff{Type: diffAdded}, want: []string{"job added"}},
		{name: "a deleted job", diff: &api.JobDiff{Type: diffDeleted}, want: []string{"job deleted"}},
		{name: "edited, an Env value hidden", diff: edited, want: []string{
			`Priority: "50" -> "60"`,
			`group web: Count: "1" -> "3"`,
			`group web, task app: Config: image: "web:1.4.2" -> "web:1.4.3"`,
			`group web, task app: Env: DB_PASSWORD: added (value not shown)`,
			`group worker: added`,
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := jobDiffLines(tt.diff); !slices.Equal(got, tt.want) {
				t.Errorf("jobDiffLines =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestJobDiffLines_Bounded(t *testing.T) {
	t.Parallel()

	diff := &api.JobDiff{Type: diffEdited}
	for i := range maxDiffLines + 5 {
		diff.Fields = append(diff.Fields, &api.FieldDiff{Type: diffEdited, Name: fmt.Sprintf("F%d", i), Old: "a", New: "b"})
	}

	lines := jobDiffLines(diff)
	if len(lines) != maxDiffLines+1 || lines[maxDiffLines] != "and 5 more" {
		t.Errorf("lines %q", lines)
	}
}

// PlanOf keeps the error and the diff of a change, and an unchanged
// resource only when the server would refuse it.
func TestPlanOf(t *testing.T) {
	t.Parallel()

	resp := &api.ReleaseApplyResponse{Changes: []*api.ReleaseResourceChange{
		{Type: diffKind, Name: diffRelease, Action: "update", JobDiff: &api.JobDiff{Type: diffEdited, Fields: []*api.FieldDiff{{Type: diffEdited, Name: "Priority", Old: "50", New: "60"}}}},
		{Type: "variable", Name: "config/web", Action: "unchanged"},
		{Type: "acl_policy", Name: diffRelease, Action: "unchanged", Error: "Permission denied"},
		{Type: diffKind, Name: "new", Action: "create", Error: "Missing driver"},
	}}

	plan := PlanOf(host.ReleaseRef{Namespace: "apps", Name: diffRelease}, resp)

	want := []host.Change{
		{Kind: diffKind, Name: diffRelease, Action: "update", Diff: []string{`Priority: "50" -> "60"`}},
		{Kind: "acl_policy", Name: diffRelease, Action: "unchanged", Error: "Permission denied"},
		{Kind: diffKind, Name: "new", Action: "create", Error: "Missing driver"},
	}

	if len(plan.Changes) != len(want) {
		t.Fatalf("changes %+v", plan.Changes)
	}

	for i, c := range plan.Changes {
		w := want[i]
		if c.Kind != w.Kind || c.Name != w.Name || c.Action != w.Action || c.Error != w.Error || !slices.Equal(c.Diff, w.Diff) {
			t.Errorf("change %d = %+v, want %+v", i, c, w)
		}
	}
}
