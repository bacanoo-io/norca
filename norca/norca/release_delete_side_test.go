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
	op, cancel, err := c.srv.releases.newOp(nil, c.token, "default", "del", 0)
	must.NoError(t, err)
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

// A resource a release recorded that somebody else deleted and created again
// under its name is not the release's: an upgrade that drops it from the pack
// leaves it, warns, and no longer lists it, while a resource that was only
// changed (drift) is still the release's and deleted (review finding ATM-7).
// The removal deleted by name whatever had the name.
func TestRelease_RemovalKeepsRecreated(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	gone, drifted, stays := releaseNamespace("rm-gone", "v1"), releaseNamespace("rm-drift", "v1"), releaseNamespace("rm-stays", "v1")
	goneVar := releaseVariable("rm/gone", "v1")
	_, err := c.apply("default", "rm", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "rm"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{gone, drifted, stays, goneVar},
	})
	must.NoError(t, err)

	c.dropNamespace("rm-gone")
	c.putNamespace(&structs.Namespace{Name: "rm-gone", Description: "foreign"})
	c.dropVariable("rm/gone")
	c.putVariable("rm/gone", "foreign")
	c.putNamespace(&structs.Namespace{Name: "rm-drift", Description: "by hand"})

	resp, err := c.apply("default", "rm", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "rm"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{releaseNamespace("rm-stays", "v1")},
	})
	must.NoError(t, err)

	must.Eq(t, "foreign", c.namespaceDescription("rm-gone"), must.Sprint("the removal deleted what somebody else created"))
	must.Eq(t, "foreign", c.variableValue("rm/gone"), must.Sprint("the removal deleted what somebody else created"))
	must.Eq(t, "<none>", c.namespaceDescription("rm-drift"), must.Sprint("a drifted resource of the release was not deleted"))
	for _, res := range []*structs.ReleaseResource{gone, goneVar} {
		must.True(t, mentions(resp.Warnings, res), must.Sprintf("no warning names %s: %q", res.Ref(), resp.Warnings))
	}
	must.Eq(t, []string{stays.Ref().String()}, recorded(c.get("default", "rm")))
}

// An uninstall leaves a resource the release recorded that somebody else
// deleted and created again under its name, and says so in its changes; a
// drifted resource of the release is deleted (review finding ATM-7).
func TestRelease_UninstallKeepsRecreated(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	gone, drifted := releaseNamespace("un-gone", "v1"), releaseNamespace("un-drift", "v1")
	goneVar := releaseVariable("un/gone", "v1")
	_, err := c.apply("default", "un", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "un"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{gone, drifted, goneVar},
	})
	must.NoError(t, err)

	c.dropNamespace("un-gone")
	c.putNamespace(&structs.Namespace{Name: "un-gone", Description: "foreign"})
	c.dropVariable("un/gone")
	c.putVariable("un/gone", "foreign")
	c.putNamespace(&structs.Namespace{Name: "un-drift", Description: "by hand"})

	resp, err := c.uninstall("default", "un", "")
	must.NoError(t, err)
	must.Nil(t, c.get("default", "un"))

	must.Eq(t, "foreign", c.namespaceDescription("un-gone"), must.Sprint("the uninstall deleted what somebody else created"))
	must.Eq(t, "foreign", c.variableValue("un/gone"), must.Sprint("the uninstall deleted what somebody else created"))
	must.Eq(t, "<none>", c.namespaceDescription("un-drift"), must.Sprint("a drifted resource of the release was not deleted"))
	for _, res := range []*structs.ReleaseResource{gone, goneVar} {
		found := false
		for _, ch := range resp.Changes {
			if ch.Type == res.Type && ch.Name == res.Ref().Name && strings.Contains(ch.Error, "not deleted") {
				found = true
			}
		}
		must.True(t, found, must.Sprintf("no change says %s was not deleted", res.Ref()))
	}
}

// A resource an atomic restore created again after the operation deleted it
// is still the release's: the restore records its new modify index, and an
// uninstall deletes it (ATM-7).
func TestRelease_RestoredStaysTheRelease(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	policy := &structs.ReleaseResource{Type: structs.ReleaseResourceACLPolicy, ACLPolicy: &structs.ACLPolicy{
		Name: "ar-policy", Rules: `namespace "default" { policy = "read" }`,
	}}
	_, err := c.apply("default", "ar", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "ar"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{policy, releaseNamespace("ar-busy", "v1"), releaseNamespace("ar-keep", "v1")},
	})
	must.NoError(t, err)

	// The upgrade drops the policy and the namespace; the namespace can't
	// be deleted, which has a Variable now, so the policy deleted before it
	// is created again.
	var resp structs.VariablesApplyResponse
	must.NoError(t, c.srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: "ar-busy", Path: "in/it"},
			Items:            map[string]string{"k": "v"},
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "ar-busy", AuthToken: c.token},
	}, &resp))
	_, err = c.apply("default", "ar", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "ar"}, Atomic: pointer.Of(true),
		Resources: []*structs.ReleaseResource{releaseNamespace("ar-keep", "v1")},
	})
	must.ErrorContains(t, err, "rolled back to revision 1")
	pol, err := c.srv.State().ACLPolicyByName(nil, "ar-policy")
	must.NoError(t, err)
	must.NotNil(t, pol, must.Sprint("the restore did not create the policy again"))

	// The uninstall deletes it.
	must.NoError(t, c.srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
		Op:           structs.VarOpDelete,
		Var:          &structs.VariableDecrypted{VariableMetadata: structs.VariableMetadata{Namespace: "ar-busy", Path: "in/it"}},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "ar-busy", AuthToken: c.token},
	}, &resp))
	un, err := c.uninstall("default", "ar", "")
	must.NoError(t, err)
	pol, err = c.srv.State().ACLPolicyByName(nil, "ar-policy")
	must.NoError(t, err)
	must.Nil(t, pol, must.Sprintf("the uninstall left the policy the restore created: %v", un.Changes))
	must.Eq(t, "<none>", c.namespaceDescription("ar-busy"))
}

func releaseQuota(name string) *structs.ReleaseResource {
	return &structs.ReleaseResource{Type: structs.ReleaseResourceQuota, Quota: &structs.QuotaSpec{
		Name:   name,
		Limits: []*structs.QuotaLimit{{Region: "global", RegionLimit: &structs.Resources{CPU: 1000, MemoryMB: 1000}}},
	}}
}

func releaseNamespaceWithQuota(name, quota string) *structs.ReleaseResource {
	res := releaseNamespace(name, "v1")
	res.Namespace.Quota = quota
	return res
}

// putVariableIn writes a Variables entry into a namespace, which keeps the
// namespace from being deleted.
func (c *releaseTestClient) putVariableIn(namespace string) {
	c.t.Helper()
	var resp structs.VariablesApplyResponse
	must.NoError(c.t, c.srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: namespace, Path: "in/it"},
			Items:            map[string]string{"k": "v"},
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: namespace, AuthToken: c.token},
	}, &resp))
	must.Nil(c.t, resp.Error)
}

// namespaceQuota is the quota a namespace names, "<none>" if the namespace
// does not exist.
func (c *releaseTestClient) namespaceQuota(name string) string {
	c.t.Helper()
	ns, err := c.srv.State().NamespaceByName(nil, name)
	must.NoError(c.t, err)
	if ns == nil {
		return "<none>"
	}
	return ns.Quota
}

// namespaceIndexes are the create and modify indexes of a namespace, 0 if
// it does not exist.
func (c *releaseTestClient) namespaceIndexes(name string) (create, modify uint64) {
	c.t.Helper()
	ns, err := c.srv.State().NamespaceByName(nil, name)
	must.NoError(c.t, err)
	if ns == nil {
		return 0, 0
	}
	return ns.CreateIndex, ns.ModifyIndex
}

// warnsQuotaInUse reports whether one of the warnings says that the quota was
// not deleted because the namespace still names it.
func warnsQuotaInUse(warnings []string, quota *structs.ReleaseResource, namespace string) bool {
	for _, w := range warnings {
		if strings.Contains(w, quota.Ref().String()+" was not deleted") && strings.Contains(w, `namespace "`+namespace+`"`) {
			return true
		}
	}
	return false
}

// A removal deletes a namespace before the quota it names, so the quota is
// never taken off a namespace: a failed atomic operation that removed both
// leaves the namespace with its quota (review findings ATM-15, ATM-15b). A
// namespace the operation could not delete is left as it was, not written at
// all; one it deleted is created again by the restore with its quota in one
// write, after the quota. The removal deleted the quota first and had to
// detach it from the namespace before.
func TestRelease_RestoreKeepsQuota(t *testing.T) {
	ci.Parallel(t)
	tests := []struct {
		name string
		// busy is the namespace a Variable keeps from being deleted: the
		// upgrade fails on it.
		busy string
		// held: a namespace of somebody else names the second quota of the
		// release: the upgrade fails on that quota, after it deleted the
		// namespaces and the first quota.
		held bool
		// deleted: the operation deleted the namespace of the quota before
		// it failed.
		deleted bool
	}{
		// The namespace of the quota is deleted, the other one fails: the
		// quota was not reached.
		{name: "namespace deleted", busy: "rq-a", deleted: true},
		// The namespace of the quota can't be deleted.
		{name: "namespace refused", busy: "rq-z"},
		// The namespace and its quota are deleted, the other quota fails.
		{name: "quota deleted", held: true, deleted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ci.Parallel(t)
			c, watch := newWatchedReleaseTest(t, "rq-z")

			_, err := c.apply("default", "rq", &structs.ReleaseApplyRequest{
				Pack: &structs.ReleasePack{Name: "rq"}, Atomic: pointer.Of(false),
				Resources: []*structs.ReleaseResource{
					releaseQuota("rq-quota"), releaseQuota("rq-held"), releaseNamespaceWithQuota("rq-z", "rq-quota"),
					releaseNamespace("rq-a", "v1"), releaseNamespace("rq-keep", "v1"),
				},
			})
			must.NoError(t, err)
			if tc.busy != "" {
				c.putVariableIn(tc.busy)
			}
			if tc.held {
				c.putNamespace(&structs.Namespace{Name: "rq-foreign", Quota: "rq-held"})
			}
			_, before := c.namespaceIndexes("rq-z")
			watch.on.Store(true)
			watch.see(c.srv)

			_, err = c.apply("default", "rq", &structs.ReleaseApplyRequest{
				Pack: &structs.ReleasePack{Name: "rq"}, Atomic: pointer.Of(true),
				Resources: []*structs.ReleaseResource{releaseNamespace("rq-keep", "v1")},
			})
			must.ErrorContains(t, err, "rolled back to revision 1")
			watch.see(c.srv)

			must.True(t, c.quotaExists("rq-quota"), must.Sprint("the quota is gone"))
			must.Eq(t, "rq-quota", c.namespaceQuota("rq-z"), must.Sprint("the namespace lost its quota"))
			quotas, _ := watch.seen()
			must.Eq(t, []string{"rq-quota"}, quotas, must.Sprint("the namespace existed without its quota"))
			create, modify := c.namespaceIndexes("rq-z")
			if tc.deleted {
				must.Greater(t, before, create, must.Sprint("the namespace was not deleted and created again"))
				must.Eq(t, create, modify, must.Sprint("the restore wrote the namespace more than once"))
			} else {
				must.Eq(t, before, modify, must.Sprint("the namespace that was not deleted was written"))
			}
			must.Eq(t, "v1", c.namespaceDescription("rq-a"))
			must.True(t, c.quotaExists("rq-held"))
			rel := c.get("default", "rq")
			must.SliceEmpty(t, rel.Status.LastOperation.Warnings)
			must.SliceContainsSubset(t, recorded(rel), []string{
				releaseQuota("rq-quota").Ref().String(), releaseNamespace("rq-z", "").Ref().String(),
			})
		})
	}
}

// A namespace that a removal or an uninstall does not delete (refused,
// another owner's now, changed) keeps the quota it names: the namespaces are
// deleted before the quotas, the quota of a namespace that stays can't be
// deleted, and the operation says which namespace still names it (review
// finding ATM-15b). The release keeps the quota it could not delete. The
// quota was deleted first, after it was detached from the namespace: a
// namespace that was then not deleted stayed without a quota.
func TestRelease_RemovalLeavesQuotaOfKeptNamespace(t *testing.T) {
	ci.Parallel(t)

	// kept checks the namespace that was not deleted and its quota; before
	// is the modify index the namespace had, 0 if somebody changed it.
	kept := func(t *testing.T, c *releaseTestClient, ns, quota *structs.ReleaseResource, before uint64) {
		t.Helper()
		must.Eq(t, quota.Quota.Name, c.namespaceQuota(ns.Namespace.Name), must.Sprint("the namespace that was not deleted lost its quota"))
		must.True(t, c.quotaExists(quota.Quota.Name), must.Sprint("the quota of a namespace that stays was deleted"))
		if before != 0 {
			_, modify := c.namespaceIndexes(ns.Namespace.Name)
			must.Eq(t, before, modify, must.Sprint("the namespace that was not deleted was written"))
		}
		rel := c.get("default", "bq")
		must.NotNil(t, rel)
		must.SliceContains(t, recorded(rel), quota.Ref().String(), must.Sprint("the release no longer lists the quota it did not delete"))
		warnings := rel.Status.LastOperation.Warnings
		must.True(t, warnsQuotaInUse(warnings, quota, ns.Namespace.Name),
			must.Sprintf("no warning says that %s is still used by %s: %q", quota.Ref(), ns.Ref(), warnings))
	}
	// install installs the namespace with its quota and returns the modify
	// index of the namespace.
	install := func(t *testing.T, c *releaseTestClient, ns, quota *structs.ReleaseResource) uint64 {
		t.Helper()
		_, err := c.apply("default", "bq", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "bq"}, Atomic: pointer.Of(false),
			Resources: []*structs.ReleaseResource{quota, ns, releaseNamespace("bq-keep", "v1")},
		})
		must.NoError(t, err)
		_, modify := c.namespaceIndexes(ns.Namespace.Name)
		return modify
	}
	// upgrade drops the namespace and the quota from the pack.
	upgrade := func(c *releaseTestClient, atomic bool) error {
		_, err := c.apply("default", "bq", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "bq"}, Atomic: pointer.Of(atomic),
			Resources: []*structs.ReleaseResource{releaseNamespace("bq-keep", "v1")},
		})
		return err
	}
	ns, quota := releaseNamespaceWithQuota("bq-ns", "bq-quota"), releaseQuota("bq-quota")

	// The namespace is deleted; the quota can't be: a namespace of somebody
	// else names it now, which keeps it.
	t.Run("quota in use", func(t *testing.T) {
		ci.Parallel(t)
		var c *releaseTestClient
		c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
			cfg.releaseBeforeDelete = func(rel string, ref *structs.ReleaseResourceRef) {
				if rel == "default/bq" && ref.Type == structs.ReleaseResourceQuota {
					c.putNamespace(&structs.Namespace{Name: "bq-other", Quota: "bq-quota"})
				}
			}
		})
		defer cleanup()
		install(t, c, ns, quota)

		must.ErrorContains(t, upgrade(c, false), `quota "bq-quota" is in use by namespace "bq-other"`)
		must.Eq(t, "<none>", c.namespaceQuota("bq-ns"))
		must.Eq(t, "bq-quota", c.namespaceQuota("bq-other"), must.Sprint("a namespace lost its quota"))
		must.True(t, c.quotaExists("bq-quota"))
		rel := c.get("default", "bq")
		must.SliceContains(t, recorded(rel), quota.Ref().String())
		must.SliceNotContains(t, recorded(rel), ns.Ref().String())
	})

	// The namespace can't be deleted, it has a Variable: a non-atomic
	// upgrade stops there, and the quota is not reached.
	t.Run("upgrade, namespace refused", func(t *testing.T) {
		ci.Parallel(t)
		c, cleanup := newReleaseTestClient(t)
		defer cleanup()
		before := install(t, c, ns, quota)
		c.putVariableIn("bq-ns")

		must.ErrorContains(t, upgrade(c, false), "deleting "+ns.Ref().String())
		must.Eq(t, "bq-quota", c.namespaceQuota("bq-ns"), must.Sprint("the namespace that was not deleted lost its quota"))
		must.True(t, c.quotaExists("bq-quota"), must.Sprint("the quota of a namespace that stays was deleted"))
		_, modify := c.namespaceIndexes("bq-ns")
		must.Eq(t, before, modify, must.Sprint("the namespace that was not deleted was written"))
		must.SliceContainsSubset(t, recorded(c.get("default", "bq")), []string{ns.Ref().String(), quota.Ref().String()})
	})

	// The same in an uninstall, which goes on to the quota.
	t.Run("uninstall, namespace refused", func(t *testing.T) {
		ci.Parallel(t)
		c, cleanup := newReleaseTestClient(t)
		defer cleanup()
		before := install(t, c, ns, quota)
		c.putVariableIn("bq-ns")

		_, err := c.uninstall("default", "bq", "")
		must.ErrorContains(t, err, "deleting "+ns.Ref().String())
		must.ErrorContains(t, err, `quota "bq-quota" is in use by namespace "bq-ns"`)
		kept(t, c, ns, quota, before)
		must.SliceContains(t, recorded(c.get("default", "bq")), ns.Ref().String())
	})

	// Another release owns the namespace when the operation comes to it: the
	// release no longer lists the namespace, and keeps the quota.
	for _, mode := range []string{"uninstall", "upgrade", "atomic upgrade"} {
		t.Run(mode+", namespace disowned", func(t *testing.T) {
			ci.Parallel(t)
			c, cleanup := newReleaseTestClient(t)
			defer cleanup()
			before := install(t, c, ns, quota)
			recordReleaseOwner(t, c.srv, "bq-owner", ns.Ref())

			var err error
			switch mode {
			case "uninstall":
				_, err = c.uninstall("default", "bq", "")
			default:
				err = upgrade(c, mode == "atomic upgrade")
			}
			must.ErrorContains(t, err, `quota "bq-quota" is in use by namespace "bq-ns"`)
			if mode == "atomic upgrade" {
				must.ErrorContains(t, err, "rolled back to revision 1")
			}
			kept(t, c, ns, quota, before)
			must.SliceNotContains(t, recorded(c.get("default", "bq")), ns.Ref().String())
		})
	}

	// The namespace is changed by hand while the uninstall runs, and can't
	// be deleted: it keeps the change and its quota.
	t.Run("uninstall, namespace changed", func(t *testing.T) {
		ci.Parallel(t)
		var c *releaseTestClient
		c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
			cfg.releaseBeforeDelete = func(rel string, ref *structs.ReleaseResourceRef) {
				if rel == "default/bq" && ref.Type == structs.ReleaseResourceNamespace && ref.Name == "bq-ns" {
					c.putNamespace(&structs.Namespace{Name: "bq-ns", Description: "by hand", Quota: "bq-quota"})
				}
			}
		})
		defer cleanup()
		install(t, c, ns, quota)
		c.putVariableIn("bq-ns")

		_, err := c.uninstall("default", "bq", "")
		must.ErrorContains(t, err, `quota "bq-quota" is in use by namespace "bq-ns"`)
		must.Eq(t, "by hand", c.namespaceDescription("bq-ns"))
		kept(t, c, ns, quota, 0)
	})
}
