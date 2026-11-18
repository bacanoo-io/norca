// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// Whether a resource of a release is gone is read from the state, not from
// the text of the error of its deletion (review finding JOG-6): the refusal
// to delete a quota names the namespace that uses it, a name somebody else
// chose.
func TestRelease_DeleteResourceRefused(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	store := c.srv.State()

	_, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app"},
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceQuota, Quota: &structs.QuotaSpec{
			Name:   "app-quota",
			Limits: []*structs.QuotaLimit{{Region: "global", RegionLimit: &structs.Resources{CPU: 1000, MemoryMB: 1000}}},
		}}},
	})
	must.NoError(t, err)
	index, err := store.LatestIndex()
	must.NoError(t, err)
	must.NoError(t, store.UpsertNamespaces(index+1, []*structs.Namespace{{Name: "nonexistent", Quota: "app-quota"}}))

	op, cancel := c.srv.releases.newOp(nil, c.token, "default", "app", 0)
	defer cancel()
	ref := &structs.ReleaseResourceRef{Type: structs.ReleaseResourceQuota, Name: "app-quota"}
	err = op.deleteResource(ref)
	must.Error(t, err, must.Sprint("a refused deletion counted as done"))
	must.StrContains(t, err.Error(), "is in use by namespace")
	quota, err := store.QuotaSpecByName(nil, "app-quota")
	must.NoError(t, err)
	must.NotNil(t, quota)

	// A token the servers don't know is refused too.
	unknown, cancelUnknown := c.srv.releases.newOp(nil, uuid.Generate(), "default", "app", 0)
	defer cancelUnknown()
	must.Error(t, unknown.deleteResource(ref))

	// A resource that is gone already is fine.
	must.NoError(t, op.deleteResource(&structs.ReleaseResourceRef{Type: structs.ReleaseResourceQuota, Name: "never-there"}))
	must.NoError(t, op.deleteResource(&structs.ReleaseResourceRef{
		Type: structs.ReleaseResourceJob, Namespace: "default", Name: "never-there"}))

	// Once the namespace no longer uses it, the quota is deleted.
	index, err = store.LatestIndex()
	must.NoError(t, err)
	must.NoError(t, store.UpsertNamespaces(index+1, []*structs.Namespace{{Name: "nonexistent"}}))
	must.NoError(t, op.deleteResource(ref))
	quota, err = store.QuotaSpecByName(nil, "app-quota")
	must.NoError(t, err)
	must.Nil(t, quota)
}

// A dry run reports the error of an object whose kind exists, even when the
// text of the error says "not found" (review finding JOG-6): only an object
// whose kind or namespace the release may still create is planned without
// the error of the endpoint.
func TestRelease_PlanObjectErrorNamingNotFound(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	kind := &structs.ReleaseResource{Type: structs.ReleaseResourceObjectKind, ObjectKind: &structs.ObjectKind{
		Group: "test.example.com", Kind: "Widget", Namespaced: true, Schema: testReleaseKindSchema,
	}}
	object := func(name, namespace string, labels map[string]string) *structs.ReleaseResource {
		return &structs.ReleaseResource{Type: structs.ReleaseResourceObject, Object: &structs.CustomObject{
			Group: "test.example.com", Kind: "Widget", Namespace: namespace, Name: name, Labels: labels,
			Spec: map[string]any{"size": 2},
		}}
	}
	plan := func(resources ...*structs.ReleaseResource) map[string]*structs.ReleaseResourceChange {
		t.Helper()
		resp, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "app"}, Resources: resources, DryRun: true,
		})
		must.NoError(t, err)
		changes := map[string]*structs.ReleaseResourceChange{}
		for _, ch := range resp.Changes {
			changes[ch.Type+":"+ch.Name] = ch
		}
		return changes
	}

	// The kind is created by the release itself: no error yet.
	changes := plan(kind, object("w1", "", nil))
	ch := changes["object:test.example.com/Widget/w1"]
	must.NotNil(t, ch)
	must.Eq(t, structs.ReleaseChangeCreate, ch.Action)
	must.Eq(t, "", ch.Error)

	_, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app"}, Resources: []*structs.ReleaseResource{kind},
	})
	must.NoError(t, err)

	// The kind exists; the label key is refused, and the refusal quotes it.
	changes = plan(kind, object("w2", "", map[string]string{"not found": "x"}))
	ch = changes["object:test.example.com/Widget/w2"]
	must.NotNil(t, ch)
	must.Eq(t, structs.ReleaseChangeCreate, ch.Action)
	must.StrContains(t, ch.Error, "invalid label key")

	// The namespace is created by the release itself: no error yet.
	ns := &structs.ReleaseResource{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "app-ns"}}
	changes = plan(kind, ns, object("w3", "app-ns", nil))
	ch = changes["object:test.example.com/Widget/w3"]
	must.NotNil(t, ch)
	must.Eq(t, structs.ReleaseChangeCreate, ch.Action)
	must.Eq(t, "", ch.Error)
}
