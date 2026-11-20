// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func (c *releaseTestClient) putQuota(q *structs.QuotaSpec) {
	c.t.Helper()
	var resp structs.GenericResponse
	must.NoError(c.t, c.srv.RPC("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
		Quotas:       []*structs.QuotaSpec{q.Copy()},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &resp))
}

// quotaExists reports whether a quota of the name exists.
func (c *releaseTestClient) quotaExists(name string) bool {
	c.t.Helper()
	q, err := c.srv.State().QuotaSpecByName(nil, name)
	must.NoError(c.t, err)
	return q != nil
}

// namespaceWatch records the states a namespace is seen in while release
// operations run: each time an operation is about to write, delete or
// restore a resource (the test hooks of the configuration), and when the
// test asks (see).
type namespaceWatch struct {
	name string
	on   atomic.Bool

	mu sync.Mutex
	// quotas are the quotas the namespace was seen with, "" for none, in
	// the order seen; indexes its modify indexes.
	quotas  []string
	indexes []uint64
}

// newWatchedReleaseTest returns a release test client and the watch of the
// namespace of the name, which records nothing until it is turned on.
func newWatchedReleaseTest(t *testing.T, namespace string) (*releaseTestClient, *namespaceWatch) {
	t.Helper()
	w := &namespaceWatch{name: namespace}
	var srv atomic.Pointer[Server]
	look := func(string, *structs.ReleaseResourceRef) {
		if s := srv.Load(); s != nil {
			w.see(s)
		}
	}
	c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
		cfg.releaseBeforeApply, cfg.releaseBeforeDelete, cfg.releaseBeforeRestore = look, look, look
	})
	t.Cleanup(cleanup)
	srv.Store(c.srv)
	return c, w
}

// see records the namespace as it is now, if it exists.
func (w *namespaceWatch) see(s *Server) {
	if !w.on.Load() {
		return
	}
	ns, err := s.State().NamespaceByName(nil, w.name)
	if err != nil || ns == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !slices.Contains(w.quotas, ns.Quota) {
		w.quotas = append(w.quotas, ns.Quota)
	}
	if !slices.Contains(w.indexes, ns.ModifyIndex) {
		w.indexes = append(w.indexes, ns.ModifyIndex)
	}
}

// seen returns the quotas and the modify indexes the namespace was seen
// with.
func (w *namespaceWatch) seen() ([]string, []uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.quotas), slices.Clone(w.indexes)
}

// A namespace of a release is written with the quota it names in one write:
// the quotas of a pack are applied before its namespaces. The namespace was
// applied first, so one that named a quota the same operation created was
// written without the quota and got it in a second write, after the quota:
// until then it existed without its limit.
func TestRelease_NamespaceWrittenWithItsQuota(t *testing.T) {
	ci.Parallel(t)
	const ns = "aw-ns"
	install := func(c *releaseTestClient, resources ...*structs.ReleaseResource) {
		c.t.Helper()
		_, err := c.apply("default", "aw", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "aw"}, Atomic: pointer.Of(false), Resources: resources,
		})
		must.NoError(c.t, err)
	}
	tests := []struct {
		name string
		// before prepares what exists before the operation that is watched.
		before func(c *releaseTestClient)
		// resources are the resources of the operation that is watched,
		// besides a Variables entry, which is applied after them.
		resources []*structs.ReleaseResource
		// quotas are the quotas the namespace is seen with, in order.
		quotas []string
		// gone is a quota the operation deletes.
		gone string
	}{
		{
			name:      "new quota, new namespace",
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "aw-quota"), releaseQuota("aw-quota")},
			quotas:    []string{"aw-quota"},
		},
		{
			name:      "new quota, namespace of the release",
			before:    func(c *releaseTestClient) { install(c, releaseNamespace(ns, "v1")) },
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "aw-quota"), releaseQuota("aw-quota")},
			quotas:    []string{"", "aw-quota"},
		},
		{
			name:      "new quota, namespace made by hand",
			before:    func(c *releaseTestClient) { c.putNamespace(&structs.Namespace{Name: ns, Description: "by hand"}) },
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "aw-quota"), releaseQuota("aw-quota")},
			quotas:    []string{"", "aw-quota"},
		},
		{
			name:      "quota of the release exists",
			before:    func(c *releaseTestClient) { c.putQuota(releaseQuota("aw-quota").Quota) },
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "aw-quota"), releaseQuota("aw-quota")},
			quotas:    []string{"aw-quota"},
		},
		{
			name:      "quota of somebody else",
			before:    func(c *releaseTestClient) { c.putQuota(releaseQuota("aw-quota").Quota) },
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "aw-quota")},
			quotas:    []string{"aw-quota"},
		},
		{
			name: "quota replaced by another one of the release",
			before: func(c *releaseTestClient) {
				install(c, releaseNamespaceWithQuota(ns, "aw-quota"), releaseQuota("aw-quota"))
			},
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "aw-other"), releaseQuota("aw-other")},
			quotas:    []string{"aw-quota", "aw-other"},
			gone:      "aw-quota",
		},
		{
			name: "quota dropped, namespace stays without one",
			before: func(c *releaseTestClient) {
				install(c, releaseNamespaceWithQuota(ns, "aw-quota"), releaseQuota("aw-quota"))
			},
			resources: []*structs.ReleaseResource{releaseNamespace(ns, "v1")},
			quotas:    []string{"aw-quota", ""},
			gone:      "aw-quota",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ci.Parallel(t)
			c, watch := newWatchedReleaseTest(t, ns)
			if tc.before != nil {
				tc.before(c)
			}
			watch.on.Store(true)
			watch.see(c.srv)
			_, existed := watch.seen()

			_, err := c.apply("default", "aw", &structs.ReleaseApplyRequest{
				Pack: &structs.ReleasePack{Name: "aw"}, Atomic: pointer.Of(false),
				Resources: append(slices.Clone(tc.resources), releaseVariable("aw/after", "v1")),
			})
			must.NoError(t, err)
			watch.see(c.srv)

			quotas, indexes := watch.seen()
			must.Eq(t, tc.quotas, quotas, must.Sprint("the quotas the namespace had while the operation ran, in order"))
			must.Eq(t, len(existed)+1, len(indexes), must.Sprintf("the operation wrote the namespace more than once: %v", indexes))
			must.Eq(t, tc.quotas[len(tc.quotas)-1], c.namespaceQuota(ns))
			if tc.gone != "" {
				must.False(t, c.quotaExists(tc.gone), must.Sprintf("the quota %q was not deleted", tc.gone))
			}
		})
	}
}

// A failed atomic operation undoes its writes in reverse order: the
// namespace it wrote with a quota first, then the quota, so the quota it
// created is not in use when the restore deletes it, and the namespace has a
// quota as long as it exists.
func TestRelease_RestoreOfNamespaceWithQuota(t *testing.T) {
	ci.Parallel(t)
	const ns = "ar-ns"
	tests := []struct {
		name string
		// before are the resources of revision 1, none: the operation that
		// fails is the install.
		before []*structs.ReleaseResource
		// resources are the resources of the operation that fails, besides
		// the job it fails on.
		resources []*structs.ReleaseResource
		// quotas are the quotas the namespace is seen with, in order; final
		// the one it has in the end, "<none>" when it does not exist.
		quotas []string
		final  string
		// made is the quota the operation creates and the restore deletes.
		made string
	}{
		{
			name:      "install",
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "ar-quota"), releaseQuota("ar-quota")},
			quotas:    []string{"ar-quota"},
			final:     "<none>",
			made:      "ar-quota",
		},
		{
			name:      "upgrade gives the namespace a quota",
			before:    []*structs.ReleaseResource{releaseNamespace(ns, "v1")},
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "ar-quota"), releaseQuota("ar-quota")},
			quotas:    []string{"", "ar-quota"},
			final:     "",
			made:      "ar-quota",
		},
		{
			name:      "upgrade replaces the quota",
			before:    []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "ar-first"), releaseQuota("ar-first")},
			resources: []*structs.ReleaseResource{releaseNamespaceWithQuota(ns, "ar-quota"), releaseQuota("ar-quota")},
			quotas:    []string{"ar-first", "ar-quota"},
			final:     "ar-first",
			made:      "ar-quota",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ci.Parallel(t)
			c, watch := newWatchedReleaseTest(t, ns)
			if tc.before != nil {
				_, err := c.apply("default", "ar", &structs.ReleaseApplyRequest{
					Pack: &structs.ReleasePack{Name: "ar"}, Atomic: pointer.Of(false), Resources: tc.before,
				})
				must.NoError(t, err)
			}
			watch.on.Store(true)
			watch.see(c.srv)

			_, err := c.apply("default", "ar", &structs.ReleaseApplyRequest{
				Pack: &structs.ReleasePack{Name: "ar"}, Atomic: pointer.Of(true),
				Resources: append(slices.Clone(tc.resources), releaseFailingJob("ar-bad")),
			})
			must.ErrorContains(t, err, "rolled back to revision")
			must.StrNotContains(t, err.Error(), "restoring the previous state failed")
			watch.see(c.srv)

			quotas, _ := watch.seen()
			must.Eq(t, tc.quotas, quotas, must.Sprint("the quotas the namespace had while the operation ran, in order"))
			must.Eq(t, tc.final, c.namespaceQuota(ns))
			must.False(t, c.quotaExists(tc.made), must.Sprintf("the restore left the quota %q", tc.made))
			must.SliceEmpty(t, c.get("default", "ar").Status.LastOperation.Warnings)
		})
	}
}

// A namespace that names a quota neither the pack has nor anybody created is
// refused by the endpoint, as before: nothing of it is written.
func TestRelease_NamespaceWithUnknownQuota(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	_, err := c.apply("default", "uq", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "uq"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{releaseNamespaceWithQuota("uq-ns", "uq-missing")},
	})
	must.ErrorContains(t, err, fmt.Sprintf("non-existent quota %q", "uq-missing"))
	must.Eq(t, "<none>", c.namespaceQuota("uq-ns"))
}
