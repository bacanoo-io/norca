// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func TestResourceClaims(t *testing.T) {
	ci.Parallel(t)
	claims := newResourceClaims()
	rel := claims.claimant(claimRelease, "default/rel")
	tenant := claims.claimant(claimTenant, "team")
	again := claims.claimant(claimTenant, "team")
	must.NotEq(t, tenant.id, again.id, must.Sprint("two passes of one tenant are one claimant"))

	must.MapEmpty(t, claims.claim(rel, []string{"namespace//a", "namespace//b"}))
	must.MapEmpty(t, claims.claim(rel, []string{"namespace//a"}), must.Sprint("a claimant conflicts with itself"))

	// What others claim is returned and not claimed; the rest is.
	others := claims.claim(tenant, []string{"namespace//b", "acl-policy//p"})
	must.MapLen(t, 1, others)
	must.Eq(t, rel, others["namespace//b"])
	must.True(t, claims.holds(tenant, "acl-policy//p"))
	must.False(t, claims.holds(tenant, "namespace//b"))
	must.MapLen(t, 1, claims.claim(again, []string{"acl-policy//p"}))
	must.Eq(t, `release "default/rel"`, others["namespace//b"].String())

	must.MapLen(t, 2, claims.holders([]string{"namespace//a", "acl-policy//p", "quota//q"}))

	claims.release(rel)
	_, ok := claims.holder("namespace//a")
	must.False(t, ok)
	must.True(t, claims.holds(tenant, "acl-policy//p"), must.Sprint("a release gave up the claims of another claimant"))
	must.MapEmpty(t, claims.claim(again, []string{"namespace//b"}))
}

// newReleaseRaceTest returns a release test client whose release operations
// run the function set with during, once, right before the release (as
// "namespace/name") writes the resource of the key target: what happens
// while the operation runs.
func newReleaseRaceTest(t *testing.T, release, target string) (*releaseTestClient, *atomic.Pointer[func()]) {
	t.Helper()
	during := &atomic.Pointer[func()]{}
	c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
		cfg.releaseBeforeApply = func(rel string, ref *structs.ReleaseResourceRef) {
			if rel != release || ref.Key() != target {
				return
			}
			if f := during.Swap(nil); f != nil {
				(*f)()
			}
		}
	})
	t.Cleanup(cleanup)
	return c, during
}

func releaseNamespace(name, description string) *structs.ReleaseResource {
	return &structs.ReleaseResource{Type: structs.ReleaseResourceNamespace,
		Namespace: &structs.Namespace{Name: name, Description: description}}
}

// Two releases that name one resource at the same time: the one that runs
// claims it, the other fails before it writes anything, and the resource
// has one owner (review finding ATM-4). Ownership was checked against the
// snapshot of the start of each operation and recorded after the writes, so
// both releases wrote the namespace and both recorded it.
func TestRelease_ConcurrentClaim(t *testing.T) {
	ci.Parallel(t)
	c, during := newReleaseRaceTest(t, "default/first", "namespace//shared")

	secondErr := make(chan error, 1)
	second := func() {
		var resp structs.ReleaseApplyResponse
		secondErr <- c.srv.RPC("Release.Apply", &structs.ReleaseApplyRequest{
			Name: "second",
			Pack: &structs.ReleasePack{Name: "second"},
			Resources: []*structs.ReleaseResource{
				releaseNamespace("shared", "second"), releaseNamespace("second-only", "second"),
			},
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
		}, &resp)
	}
	during.Store(&second)

	_, err := c.apply("default", "first", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "first"},
		Resources: []*structs.ReleaseResource{releaseNamespace("shared", "first")},
	})
	must.NoError(t, err)
	must.Nil(t, during.Load(), must.Sprint("the second release did not run during the first"))
	err = <-secondErr

	must.SliceContains(t, recorded(c.get("default", "first")), `namespace "shared"`)
	if rel := c.get("default", "second"); rel != nil {
		must.SliceNotContains(t, recorded(rel), `namespace "shared"`, must.Sprint("two releases own the namespace"))
	}
	must.ErrorContains(t, err, `namespace "shared" is being written by release "default/first"`)

	// The second release wrote nothing.
	ns, err := c.srv.State().NamespaceByName(nil, "shared")
	must.NoError(t, err)
	must.Eq(t, "first", ns.Description)
	ns, err = c.srv.State().NamespaceByName(nil, "second-only")
	must.NoError(t, err)
	must.Nil(t, ns, must.Sprint("the release that lost the claim wrote a resource"))
}

// A tenant whose pass runs while a release writes a resource the tenant
// wants does not take it: the release claimed it, the tenant reports the
// conflict and writes nothing of it, and the resource has one owner (review
// finding ATM-4). The pass knew only the resources releases had recorded
// after their writes, so the tenant created the resource, and the release
// then wrote it too and recorded it: two owners.
func TestTenant_ReleaseClaimsConcurrently(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name     string
		tenant   string
		spec     map[string]any
		target   tenantResourceRef
		resource *structs.ReleaseResource
		exists   func(c *certTest) bool
	}{
		{
			name:     "namespace",
			tenant:   "team-x",
			spec:     map[string]any{"namespace": map[string]any{"description": "Tenant X"}},
			target:   tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "team-x"},
			resource: releaseNamespace("team-x", "from the release"),
			exists: func(c *certTest) bool {
				ns, err := c.s.State().NamespaceByName(nil, "team-x")
				must.NoError(c.t, err)
				return ns != nil && ns.Description == "from the release"
			},
		},
		{
			name:   "ACL policy",
			tenant: "team-y",
			spec:   map[string]any{"roles": map[string]any{"developer": []any{"submit-job"}}},
			target: tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: "team-y-developer"},
			resource: &structs.ReleaseResource{Type: structs.ReleaseResourceACLPolicy, ACLPolicy: &structs.ACLPolicy{
				Name: "team-y-developer", Description: "from the release", Rules: `namespace "team-y" { policy = "read" }`,
			}},
			exists: func(c *certTest) bool {
				pol, err := c.s.State().ACLPolicyByName(nil, "team-y-developer")
				must.NoError(c.t, err)
				return pol != nil && pol.Description == "from the release"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ci.Parallel(t)
			rc, during := newReleaseRaceTest(t, "default/rel", tc.target.key())
			c := &certTest{t: t, s: rc.srv, token: rc.token}
			waitTenantKind(t, rc.srv)

			// While the release runs, the tenant is created and its first
			// pass ends (ready or in conflict).
			type seen struct {
				err     error
				created bool
				status  map[string]any
			}
			result := make(chan seen, 1)
			tenant := func() {
				out := seen{}
				defer func() { result <- out }()
				if _, out.err = c.apply("", structs.TenantKind, tc.tenant, tc.spec); out.err != nil {
					return
				}
				deadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(deadline) {
					obj := c.get("", structs.TenantKind, tc.tenant)
					if obj != nil && (tenantReady(obj) || objectConditionTrue(obj.Status, structs.TenantConditionConflict)) {
						out.status = obj.Status
						// What exists now the tenant wrote: the release
						// has not written the resource yet.
						switch tc.target.Type {
						case structs.TenantResourceNamespace:
							ns, _ := c.s.State().NamespaceByName(nil, tc.target.Name)
							out.created = ns != nil
						case structs.TenantResourceACLPolicy:
							pol, _ := c.s.State().ACLPolicyByName(nil, tc.target.Name)
							out.created = pol != nil
						}
						return
					}
					time.Sleep(50 * time.Millisecond)
				}
				out.err = fmt.Errorf("the first pass of the tenant did not end: %v", c.get("", structs.TenantKind, tc.tenant))
			}
			during.Store(&tenant)

			_, err := rc.apply("default", "rel", &structs.ReleaseApplyRequest{
				Pack:      &structs.ReleasePack{Name: "rel"},
				Resources: []*structs.ReleaseResource{tc.resource},
			})
			must.NoError(t, err)
			must.Nil(t, during.Load(), must.Sprint("the tenant was not created during the release"))
			first := <-result
			must.NoError(t, first.err)

			final := waitTenant(c, tc.tenant, "in conflict", func(o *structs.CustomObject) bool {
				return objectConditionTrue(o.Status, structs.TenantConditionConflict)
			})
			must.SliceContains(t, recorded(rc.get("default", "rel")), tc.resource.Ref().String())
			must.SliceNotContains(t, tenantResources(final), tc.target.key(),
				must.Sprint("the tenant and the release both own the resource"))
			must.False(t, first.created, must.Sprintf("the tenant wrote the resource the release was writing: %v", first.status))
			must.True(t, objectConditionTrue(first.status, structs.TenantConditionConflict),
				must.Sprintf("the first pass does not report the conflict: %v", first.status))
			must.True(t, tc.exists(c))
		})
	}
}

// A release operation re-checks the owner of each resource right before it
// writes it, in the state as it is then: an owner the claims do not show (a
// release of an operation that the claims of this leader do not include)
// makes the operation fail before it writes the resource (ATM-4).
func TestRelease_OwnerBeforeWrite(t *testing.T) {
	ci.Parallel(t)
	c, during := newReleaseRaceTest(t, "default/late", "namespace//late-ns")
	other := func() {
		// Past the claims, as an operation of another leader would.
		_, err := c.srv.releases.writeRelease(&structs.Release{
			ID: uuid.Generate(), Namespace: "default", Name: "other",
			Spec: &structs.ReleaseSpec{MaxHistory: structs.DefaultReleaseMaxHistory, Revision: 1},
			Status: &structs.ReleaseStatus{Phase: structs.ReleasePhaseDeployed, Revision: 1, Resources: []*structs.ReleaseResourceRef{
				{Type: structs.ReleaseResourceNamespace, Name: "late-ns"},
			}},
		}, 0)
		must.NoError(t, err)
	}
	during.Store(&other)

	_, err := c.apply("default", "late", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "late"},
		Resources: []*structs.ReleaseResource{releaseNamespace("late-ns", "late")},
	})
	must.Nil(t, during.Load(), must.Sprint("the other release was not written during the operation"))
	must.ErrorContains(t, err, `namespace "late-ns" belongs to release "default/other" now`)
	ns, err := c.srv.State().NamespaceByName(nil, "late-ns")
	must.NoError(t, err)
	must.Nil(t, ns, must.Sprint("the release wrote a resource of another release"))
	must.SliceNotContains(t, recorded(c.get("default", "late")), `namespace "late-ns"`)
}

// A tenant does not take a resource a release operation claims, and keeps it
// if it lists it; a resource a release owns is not the tenant's even if the
// tenant lists it (ATM-4). The tenant's own list was asked before the
// owners releases recorded.
func TestTenant_TakeClaimedByRelease(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	o := newTenantOperator(c.s, c.s.logger)
	p := newTenantPass(t, c.s)
	claims := c.s.releases.claims

	tenant := func(listed tenantResourceRef) *tenantWork {
		obj := &structs.CustomObject{Group: structs.OperatorsGroup, Kind: structs.TenantKind, Name: "team"}
		w := &tenantWork{o: o, p: p, obj: obj, spec: &tenantSpec{}, owned: map[string]tenantResourceRef{listed.key(): listed}}
		w.claimant = claims.claimant(claimTenant, "team")
		return w
	}

	// Claimed by a running release operation: not written in this pass,
	// still listed.
	running := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "running"}
	op := claims.claimant(claimRelease, "default/rel")
	claims.claim(op, []string{running.key()})
	defer claims.release(op)
	w := tenant(running)
	must.False(t, w.take(running, &tenantOriginal{}, &tenantOriginal{}))
	must.True(t, w.owns(running))
	must.StrContains(t, w.conflicts[0], `is being written by release "default/rel"`)

	// Recorded by a release: given up.
	recordedRef := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "recorded"}
	p.releases[recordedRef.key()] = "default/rel"
	w = tenant(recordedRef)
	must.False(t, w.take(recordedRef, &tenantOriginal{}, &tenantOriginal{}))
	must.False(t, w.owns(recordedRef), must.Sprint("the tenant still lists a resource of a release"))
	must.StrContains(t, w.conflicts[0], "belongs to the release default/rel")
}
