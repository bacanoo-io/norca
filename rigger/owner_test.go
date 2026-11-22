// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// The owner of a release is a stack by namespace and name: the stack of the
// same name in another namespace neither prunes nor lists nor destroys it.
func TestStack_OwnerHasANamespace(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := stackEngine(t, f)

	if _, diags := e.ApplyStack(t.Context(), planOf(t, e, stackFiles(twoReleases), nil), rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	other := stackFiles(strings.Replace(twoReleases, `namespace = "apps"`, `namespace = "other"`, 1))

	if got := actions(planOf(t, e, other, nil)); got != bothInstalled {
		t.Errorf("plan of other/shop = %s", got)
	}

	s, diags := e.LoadStack(t.Context(), other, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if status, err := e.StackStatus(t.Context(), s); err != nil || len(status) != 0 {
		t.Errorf("status of other/shop = %v, %v", status, err)
	}

	if removed, err := e.DestroyStack(t.Context(), s); err != nil || len(removed) != 0 {
		t.Errorf("destroy of other/shop removed %v, %v", removed, err)
	}
}

// A release of another stack is a take-over, also with the same content:
// for a stack and for an install of one module.
func TestStack_ForeignOwner(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := stackEngine(t, f)

	if _, diags := e.ApplyStack(t.Context(), planOf(t, e, stackFiles(twoReleases), nil), rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	second := stackFiles(strings.Replace(twoReleases, `name      = "shop"`, `name      = "second"`, 1))
	plan := planOf(t, e, second, nil)

	for _, rp := range plan.Releases {
		if rp.Action != rigger.ActionUpgrade || !strings.Contains(rp.TakesOver, "apps/shop") {
			t.Errorf("plan of %s = %s, takes over %q", rp.Release.Name, rp.Action, rp.TakesOver)
		}
	}

	if _, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{}); !strings.Contains(diags.Error(), "-adopt") {
		t.Errorf("apply without adopt = %v", diags)
	}

	m, diags := e.LoadModule(t.Context(), fstest.MapFS{metadataFile: pathsMetadata("db")})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	rp, diags := e.PlanInstall(t.Context(), m, rigger.Install{Release: "db", Namespace: testNamespace, Region: "eu"}, host.Settings{})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if !strings.Contains(rp.TakesOver, "apps/shop") {
		t.Errorf("an install onto db takes over %q", rp.TakesOver)
	}
}
