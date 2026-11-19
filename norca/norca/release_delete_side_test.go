// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// The writes of somebody else, through the endpoints, with the root token
// of a release test client.

func (c *releaseTestClient) putNamespace(ns *structs.Namespace) {
	c.t.Helper()
	var resp structs.GenericResponse
	must.NoError(c.t, c.srv.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces:   []*structs.Namespace{ns},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &resp))
}

func (c *releaseTestClient) dropNamespace(name string) {
	c.t.Helper()
	var resp structs.GenericResponse
	must.NoError(c.t, c.srv.RPC("Namespace.DeleteNamespaces", &structs.NamespaceDeleteRequest{
		Namespaces:   []string{name},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &resp))
}

func (c *releaseTestClient) putVariable(path, value string) {
	c.t.Helper()
	var resp structs.VariablesApplyResponse
	must.NoError(c.t, c.srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: path},
			Items:            map[string]string{"k": value},
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &resp))
	must.Nil(c.t, resp.Error)
}

func (c *releaseTestClient) dropVariable(path string) {
	c.t.Helper()
	var resp structs.VariablesApplyResponse
	must.NoError(c.t, c.srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
		Op:           structs.VarOpDelete,
		Var:          &structs.VariableDecrypted{VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: path}},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &resp))
	must.Nil(c.t, resp.Error)
}

// namespaceDescription is the description of a namespace, "<none>" if it
// does not exist.
func (c *releaseTestClient) namespaceDescription(name string) string {
	c.t.Helper()
	ns, err := c.srv.State().NamespaceByName(nil, name)
	must.NoError(c.t, err)
	if ns == nil {
		return "<none>"
	}
	return ns.Description
}

// variableValue is the item k of a Variables entry of the namespace
// default, "<none>" if it does not exist.
func (c *releaseTestClient) variableValue(path string) string {
	c.t.Helper()
	snap, err := c.srv.State().Snapshot()
	must.NoError(c.t, err)
	live, err := c.srv.releases.readLive(snap, &structs.ReleaseResourceRef{
		Type: structs.ReleaseResourceVariable, Namespace: "default", Name: path})
	must.NoError(c.t, err)
	if !live.exists() {
		return "<none>"
	}
	return live.res.Variable.Items["k"]
}

func releaseVariable(path, value string) *structs.ReleaseResource {
	return &structs.ReleaseResource{Type: structs.ReleaseResourceVariable, Variable: &structs.VariableDecrypted{
		VariableMetadata: structs.VariableMetadata{Path: path},
		Items:            map[string]string{"k": value},
	}}
}

// releaseFailingJob is a job whose register fails validation.
func releaseFailingJob(id string) *structs.ReleaseResource {
	job := testReleaseJob(id)
	job.TaskGroups[0].Tasks[0].Resources.CPU = -5
	return &structs.ReleaseResource{Type: structs.ReleaseResourceJob, Job: job}
}

// mentions reports whether one of the texts names the resource.
func mentions(texts []string, res *structs.ReleaseResource) bool {
	for _, text := range texts {
		if strings.Contains(text, res.Ref().String()) {
			return true
		}
	}
	return false
}

// A failed atomic operation restores only what is still as it left it
// (review finding ATM-6). What somebody else changed after the operation
// wrote it, and what somebody else created after deleting what the
// operation created, stay as they are, and the operation warns about them.
// The restore wrote back the state before the operation unconditionally and
// deleted what the operation created by name.
func TestRelease_RestoreKeepsLaterChanges(t *testing.T) {
	ci.Parallel(t)
	c, during := newReleaseRaceTest(t, "default/restore", "job/default/restore-bad")

	kept, keptVar := releaseNamespace("restore-kept", "v1"), releaseVariable("restore/kept", "v1")
	_, err := c.apply("default", "restore", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "restore"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{kept, keptVar},
	})
	must.NoError(t, err)

	// The upgrade changes the namespace and the Variable of revision 1,
	// creates a namespace and a Variable, and fails on the job. Before the
	// job, somebody else changes the first two and replaces the others.
	made, madeVar := releaseNamespace("restore-made", "release"), releaseVariable("restore/made", "release")
	meanwhile := func() {
		c.putNamespace(&structs.Namespace{Name: "restore-kept", Description: "by hand"})
		c.putVariable("restore/kept", "by hand")
		c.dropNamespace("restore-made")
		c.putNamespace(&structs.Namespace{Name: "restore-made", Description: "foreign"})
		c.dropVariable("restore/made")
		c.putVariable("restore/made", "foreign")
	}
	during.Store(&meanwhile)
	_, err = c.apply("default", "restore", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "restore"}, Atomic: pointer.Of(true),
		Resources: []*structs.ReleaseResource{
			releaseNamespace("restore-kept", "v2"), made, releaseVariable("restore/kept", "v2"), madeVar,
			releaseFailingJob("restore-bad"),
		},
	})
	must.Nil(t, during.Load(), must.Sprint("nothing changed while the operation ran"))
	must.ErrorContains(t, err, "rolled back to revision 1")

	must.Eq(t, "by hand", c.namespaceDescription("restore-kept"), must.Sprint("the restore overwrote a later change"))
	must.Eq(t, "by hand", c.variableValue("restore/kept"), must.Sprint("the restore overwrote a later change"))
	must.Eq(t, "foreign", c.namespaceDescription("restore-made"), must.Sprint("the restore deleted what somebody else created"))
	must.Eq(t, "foreign", c.variableValue("restore/made"), must.Sprint("the restore deleted what somebody else created"))

	rel := c.get("default", "restore")
	warnings := rel.Status.LastOperation.Warnings
	for _, res := range []*structs.ReleaseResource{kept, keptVar, made, madeVar} {
		must.True(t, mentions(warnings, res), must.Sprintf("no warning names %s: %q", res.Ref(), warnings))
	}
	must.SliceNotContains(t, recorded(rel), made.Ref().String(), must.Sprint("the release owns what somebody else created"))
}

// The restore writes back with check-and-set against what it read right
// before: a change committed between that read and the write is kept, and
// the operation warns (ATM-6).
func TestRelease_RestoreChecksWhatItRead(t *testing.T) {
	ci.Parallel(t)
	var c *releaseTestClient
	c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
		cfg.releaseBeforeRestore = func(rel string, ref *structs.ReleaseResourceRef) {
			switch {
			case rel != "default/window":
			case ref.Type == structs.ReleaseResourceNamespace && ref.Name == "window-ns":
				c.putNamespace(&structs.Namespace{Name: "window-ns", Description: "in the window"})
			case ref.Type == structs.ReleaseResourceVariable && ref.Name == "window/v":
				c.putVariable("window/v", "in the window")
			}
		}
	})
	defer cleanup()

	ns, v := releaseNamespace("window-ns", "v1"), releaseVariable("window/v", "v1")
	_, err := c.apply("default", "window", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "window"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{ns, v},
	})
	must.NoError(t, err)
	_, err = c.apply("default", "window", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "window"}, Atomic: pointer.Of(true),
		Resources: []*structs.ReleaseResource{
			releaseNamespace("window-ns", "v2"), releaseVariable("window/v", "v2"), releaseFailingJob("window-bad"),
		},
	})
	must.ErrorContains(t, err, "rolled back to revision 1")
	must.Eq(t, "in the window", c.namespaceDescription("window-ns"))
	must.Eq(t, "in the window", c.variableValue("window/v"))
	warnings := c.get("default", "window").Status.LastOperation.Warnings
	for _, res := range []*structs.ReleaseResource{ns, v} {
		must.True(t, mentions(warnings, res), must.Sprintf("no warning names %s: %q", res.Ref(), warnings))
	}
}

// The restore of a deletion creates the resource again only while nothing
// of its name exists: one somebody else created since is kept (ATM-6).
func TestRelease_RestoreOfDeletion(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	op, cancel := c.srv.releases.newOp(nil, c.token, "default", "del", 0)
	defer cancel()

	// What the operation deleted, as it read it before.
	deleted := func(name string) *appliedChange {
		c.putNamespace(&structs.Namespace{Name: name, Description: "release"})
		snap, err := c.srv.State().Snapshot()
		must.NoError(t, err)
		ref := &structs.ReleaseResourceRef{Type: structs.ReleaseResourceNamespace, Name: name}
		before, err := c.srv.releases.readLive(snap, ref)
		must.NoError(t, err)
		c.dropNamespace(name)
		return &appliedChange{ref: ref, before: before, deleted: true}
	}
	gone, taken := deleted("del-gone"), deleted("del-taken")
	c.putNamespace(&structs.Namespace{Name: "del-taken", Description: "foreign"})
	op.applied = []*appliedChange{gone, taken}

	must.SliceEmpty(t, op.restore())
	must.Eq(t, "release", c.namespaceDescription("del-gone"), must.Sprint("a deletion was not restored"))
	must.Eq(t, "foreign", c.namespaceDescription("del-taken"), must.Sprint("the restore overwrote what somebody else created"))
	must.SliceLen(t, 1, op.warnings)
	must.StrContains(t, op.warnings[0], `namespace "del-taken"`)
}
