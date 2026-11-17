// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// newTenantTest returns a test server with ACLs, driven with its root token.
func newTenantTest(t *testing.T) (*certTest, func()) {
	t.Helper()
	s, root, cleanup := TestACLServer(t, nil)
	testutil.WaitForLeader(t, s.RPC)
	waitTenantKind(t, s)
	return &certTest{t: t, s: s, token: root.SecretID}, cleanup
}

// waitTenantKind waits for the kind Tenant. The leader registers the
// built-in kinds after it is elected, on its own time: a tenant written
// before that is refused.
func waitTenantKind(t *testing.T, s *Server) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		kind, err := s.State().ObjectKindByName(nil, structs.OperatorsGroup, structs.TenantKind)
		return err == nil && kind != nil
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the kind Tenant is registered"))
}

// waitTenant waits until the tenant satisfies cond.
func waitTenant(c *certTest, name, what string, cond func(*structs.CustomObject) bool) *structs.CustomObject {
	c.t.Helper()
	var obj *structs.CustomObject
	must.Wait(c.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			obj = c.get("", structs.TenantKind, name)
			return obj != nil && cond(obj)
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	), must.Func(func() string {
		// Evaluated when the wait failed: the object as it is then.
		return fmt.Sprintf("waiting for the tenant %s: %s; object: %v", name, what, c.get("", structs.TenantKind, name))
	}))
	return obj
}

func tenantReady(obj *structs.CustomObject) bool {
	return obj.Status["observedGeneration"] != nil &&
		objectConditionTrue(obj.Status, structs.CertificateConditionReady) &&
		toInt64(obj.Status["observedGeneration"]) == int64(obj.Generation)
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case uint64:
		return int64(n)
	}
	return -1
}

func tenantResources(obj *structs.CustomObject) []string {
	st := &tenantStatus{}
	_ = structs.DecodeObjectPart(obj.Status, st)
	var out []string
	for _, r := range st.Resources {
		out = append(out, r.key())
	}
	return out
}

func TestTenant_Lifecycle(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	s := c.s
	store := s.State()

	_, err := c.apply("", structs.TenantKind, "team-a", map[string]any{
		"namespace": map[string]any{"description": "Team A", "meta": map[string]any{"owner": "a"}},
		"quota": map[string]any{"region": map[string]any{
			"global": map[string]any{"cpu": 8000, "memory": 16384},
		}},
		"roles": map[string]any{
			"developer": []any{"submit-job", "read-logs"},
			"viewer":    map[string]any{"capabilities": []any{"read-job"}, "policy": `node { policy = "read" }`},
		},
		"network":                  map[string]any{"isolation": "namespace", "allow_from_namespaces": []any{"ingress"}},
		"certificate_dns_suffixes": []any{"team-a.example.com"},
		"certificate_issuers":      []any{"letsencrypt"},
		"dns_suffixes":             []any{"team-a.example.com", "*.apps.example.com"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-a", "ready", tenantReady)
	must.SliceContains(t, obj.Finalizers, structs.OperatorsFinalizer)
	must.SliceContainsAll(t, tenantResources(obj), []string{
		"namespace//team-a", "quota//team-a", "acl-policy//team-a-developer", "acl-role//team-a-developer",
		"acl-policy//team-a-viewer", "acl-role//team-a-viewer", "network-policy/team-a/tenant-isolation",
	})

	ns, err := store.NamespaceByName(nil, "team-a")
	must.NoError(t, err)
	must.NotNil(t, ns)
	must.Eq(t, "Team A", ns.Description)
	must.Eq(t, "team-a", ns.Quota)
	must.Eq(t, "a", ns.Meta["owner"])
	must.Eq(t, "team-a.example.com", ns.Meta[structs.NamespaceMetaCertificateDNSSuffixes])
	must.Eq(t, "letsencrypt", ns.Meta[structs.NamespaceMetaCertificateIssuers])
	must.Eq(t, "team-a.example.com,*.apps.example.com", ns.Meta[structs.NamespaceMetaDNSSuffixes])

	q, err := store.QuotaSpecByName(nil, "team-a")
	must.NoError(t, err)
	must.NotNil(t, q)
	must.Eq(t, 8000, q.Limits[0].RegionLimit.CPU)

	pol, err := store.ACLPolicyByName(nil, "team-a-developer")
	must.NoError(t, err)
	must.NotNil(t, pol)
	must.StrContains(t, pol.Rules, `namespace "team-a"`)
	must.StrContains(t, pol.Rules, `"submit-job"`)
	viewer, err := store.ACLPolicyByName(nil, "team-a-viewer")
	must.NoError(t, err)
	must.StrContains(t, viewer.Rules, `node { policy = "read" }`)
	role, err := store.GetACLRoleByName(nil, "team-a-developer")
	must.NoError(t, err)
	must.NotNil(t, role)
	must.Eq(t, "team-a-developer", role.Policies[0].Name)

	np, err := store.NetworkPolicyByName(nil, "team-a", structs.TenantNetworkPolicyName)
	must.NoError(t, err)
	must.NotNil(t, np)
	must.Len(t, 1, np.Ingress)
	must.Len(t, 2, np.Ingress[0].Peers)
	must.Eq(t, "ingress", np.Ingress[0].Peers[1].Namespace)

	// A pass with nothing changed writes nothing: no endless rewrite, of
	// the tenant or of any of its resources.
	indexes := func() map[string]uint64 {
		ns, _ := store.NamespaceByName(nil, "team-a")
		q, _ := store.QuotaSpecByName(nil, "team-a")
		pol, _ := store.ACLPolicyByName(nil, "team-a-viewer")
		role, _ := store.GetACLRoleByName(nil, "team-a-viewer")
		np, _ := store.NetworkPolicyByName(nil, "team-a", structs.TenantNetworkPolicyName)
		return map[string]uint64{
			"tenant":         c.get("", structs.TenantKind, "team-a").ModifyIndex,
			"namespace":      ns.ModifyIndex,
			"quota":          q.ModifyIndex,
			"acl-policy":     pol.ModifyIndex,
			"acl-role":       role.ModifyIndex,
			"network-policy": np.ModifyIndex,
		}
	}
	before := indexes()
	time.Sleep(2 * time.Second)
	must.Eq(t, before, indexes())

	// A change through the namespace API is set back.
	changed := ns.Copy()
	changed.Description = "changed by hand"
	var gen structs.GenericResponse
	must.NoError(t, s.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces: []*structs.Namespace{changed}, WriteRequest: c.write(""),
	}, &gen))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		ns, _ := store.NamespaceByName(nil, "team-a")
		return ns != nil && ns.Description == "Team A"
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))

	// A role taken out of the spec takes its policy and role with it.
	_, err = c.apply("", structs.TenantKind, "team-a", map[string]any{
		"namespace": map[string]any{"description": "Team A", "meta": map[string]any{"owner": "a"}},
		"quota": map[string]any{"region": map[string]any{
			"global": map[string]any{"cpu": 8000, "memory": 16384},
		}},
		"roles":   map[string]any{"developer": []any{"submit-job", "read-logs"}},
		"network": map[string]any{"isolation": "namespace", "allow_from_namespaces": []any{"ingress"}},
	})
	must.NoError(t, err)
	obj = waitTenant(c, "team-a", "the viewer role gone", func(o *structs.CustomObject) bool {
		return tenantReady(o) && !strings.Contains(strings.Join(tenantResources(o), ","), "team-a-viewer")
	})
	viewer, _ = store.ACLPolicyByName(nil, "team-a-viewer")
	must.Nil(t, viewer)
	vrole, _ := store.GetACLRoleByName(nil, "team-a-viewer")
	must.Nil(t, vrole)
	ns, _ = store.NamespaceByName(nil, "team-a")
	must.MapNotContainsKey(t, ns.Meta, structs.NamespaceMetaCertificateDNSSuffixes)

	// A job in the namespace holds the deletion back.
	job := mock.Job()
	job.Namespace = "team-a"
	var jobResp structs.JobRegisterResponse
	must.NoError(t, s.RPC("Job.Register", &structs.JobRegisterRequest{Job: job, WriteRequest: c.write("team-a")}, &jobResp))
	must.NoError(t, c.delete("", structs.TenantKind, "team-a"))
	waitTenant(c, "team-a", "waiting for the namespace", func(o *structs.CustomObject) bool {
		return objectConditionTrue(o.Status, structs.TenantConditionNamespaceNotEmpty)
	})
	ns, _ = store.NamespaceByName(nil, "team-a")
	must.NotNil(t, ns)

	var deregResp structs.JobDeregisterResponse
	must.NoError(t, s.RPC("Job.Deregister", &structs.JobDeregisterRequest{
		JobID: job.ID, Purge: true, WriteRequest: c.write("team-a"),
	}, &deregResp))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("", structs.TenantKind, "team-a") == nil
	}), wait.Timeout(60*time.Second), wait.Gap(100*time.Millisecond)),
		must.Func(func() string {
			return fmt.Sprintf("tenant still there: %v", c.get("", structs.TenantKind, "team-a"))
		}))

	ns, _ = store.NamespaceByName(nil, "team-a")
	must.Nil(t, ns)
	q, _ = store.QuotaSpecByName(nil, "team-a")
	must.Nil(t, q)
	pol, _ = store.ACLPolicyByName(nil, "team-a-developer")
	must.Nil(t, pol)
	role, _ = store.GetACLRoleByName(nil, "team-a-developer")
	must.Nil(t, role)
	np, _ = store.NetworkPolicyByName(nil, "team-a", structs.TenantNetworkPolicyName)
	must.Nil(t, np)
}

func TestTenant_ConflictAndAdopt(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()

	var gen structs.GenericResponse
	must.NoError(t, c.s.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces: []*structs.Namespace{{Name: "taken", Description: "made by hand"}}, WriteRequest: c.write(""),
	}, &gen))

	spec := map[string]any{"namespace": map[string]any{"name": "taken", "description": "Tenant B"}}
	_, err := c.apply("", structs.TenantKind, "team-b", spec)
	must.NoError(t, err)
	obj := waitTenant(c, "team-b", "conflict", func(o *structs.CustomObject) bool {
		return objectConditionTrue(o.Status, structs.TenantConditionConflict)
	})
	must.False(t, objectConditionTrue(obj.Status, structs.CertificateConditionReady))
	must.SliceEmpty(t, tenantResources(obj))
	ns, _ := store.NamespaceByName(nil, "taken")
	must.Eq(t, "made by hand", ns.Description)

	// With adopt the tenant takes the namespace over.
	spec["adopt"] = []any{"namespace"}
	_, err = c.apply("", structs.TenantKind, "team-b", spec)
	must.NoError(t, err)
	obj = waitTenant(c, "team-b", "adopted", tenantReady)
	must.SliceContains(t, tenantResources(obj), "namespace//taken")
	ns, _ = store.NamespaceByName(nil, "taken")
	must.Eq(t, "Tenant B", ns.Description)

	// A second tenant can't take the namespace.
	_, err = c.apply("", structs.TenantKind, "team-c", map[string]any{"namespace": map[string]any{"name": "taken"}})
	must.ErrorContains(t, err, `belongs to the tenant "team-b"`)
}

func TestTenant_Releases(t *testing.T) {
	ci.Parallel(t)
	rc, cleanup := newReleaseTestClient(t)
	defer cleanup()
	c := &certTest{t: t, s: rc.srv, token: rc.token}
	// newReleaseTestClient waits for the kinds of releases only.
	waitTenantKind(t, rc.srv)

	// A namespace of a release is never the tenant's, adopt or not.
	_, err := rc.apply("default", "rel", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "rel"},
		Resources: []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "rel-ns"}},
		},
	})
	must.NoError(t, err)
	_, err = c.apply("", structs.TenantKind, "t-rel", map[string]any{
		"namespace": map[string]any{"name": "rel-ns"}, "adopt": []any{"namespace"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "t-rel", "conflict", func(o *structs.CustomObject) bool {
		return objectConditionTrue(o.Status, structs.TenantConditionConflict)
	})
	var messages []string
	for _, cond := range operatorConditions(obj.Status) {
		messages = append(messages, cond.Message)
	}
	must.StrContains(t, strings.Join(messages, " "), `belongs to the release default/rel`)

	// A release can't take a namespace of a tenant.
	_, err = c.apply("", structs.TenantKind, "t-own", map[string]any{"namespace": map[string]any{"name": "own-ns"}})
	must.NoError(t, err)
	waitTenant(c, "t-own", "ready", tenantReady)
	_, err = rc.apply("default", "grab", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "grab"},
		Resources: []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "own-ns"}},
		},
		TakeOwnership: true,
	})
	must.ErrorContains(t, err, `belongs to the tenant "t-own"`)
}

func TestTenant_WriteChecks(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()

	cases := []struct {
		name string
		spec map[string]any
		want string
	}{
		{"default namespace", map[string]any{"namespace": map[string]any{"name": "default"}}, "can't belong to a tenant"},
		{"bad capability", map[string]any{"roles": map[string]any{"dev": []any{"no-such-capability"}}}, `role "dev"`},
		{"empty role", map[string]any{"roles": map[string]any{"dev": []any{}}}, "no capabilities and no policy"},
		{"bad policy", map[string]any{"roles": map[string]any{"dev": map[string]any{"policy": "namespace {"}}}, `role "dev"`},
		{"certificate meta by hand", map[string]any{"namespace": map[string]any{
			"meta": map[string]any{structs.NamespaceMetaCertificateDNSSuffixes: "x"}}}, "certificate_dns_suffixes"},
		{"every namespace", map[string]any{"network": map[string]any{
			"isolation": "namespace", "allow_from_namespaces": []any{"*"}}}, "allow_from_namespaces"},
		{"bad address", map[string]any{"certificate_ip_addresses": []any{"not-an-ip"}}, "certificate_ip_addresses"},
		{"dns meta by hand", map[string]any{"namespace": map[string]any{
			"meta": map[string]any{structs.NamespaceMetaDNSSuffixes: "x"}}}, "dns_suffixes"},
		{"bad dns suffix", map[string]any{"dns_suffixes": []any{"not a name"}}, "dns_suffixes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.apply("", structs.TenantKind, "checks", tc.spec)
			must.ErrorContains(t, err, tc.want)
		})
	}

	_, err := c.apply("", structs.TenantKind, "bad.name", map[string]any{})
	must.ErrorContains(t, err, "the name of a tenant")

	_, err = c.apply("", structs.TenantKind, "moving", map[string]any{"namespace": map[string]any{"name": "first"}})
	must.NoError(t, err)
	_, err = c.apply("", structs.TenantKind, "moving", map[string]any{"namespace": map[string]any{"name": "second"}})
	must.ErrorContains(t, err, "can't be changed")

	err = c.status("", structs.TenantKind, "moving", map[string]any{"namespace": "x"})
	must.Error(t, err)
	code, _, ok := structs.CodeFromRPCCodedErr(err)
	must.True(t, ok)
	must.Eq(t, http.StatusForbidden, code)
}

func TestTenantSpec_Desired(t *testing.T) {
	ci.Parallel(t)
	spec, err := decodeTenantSpec("t1", map[string]any{
		"roles": map[string]any{"ops": []any{"read-job", "alloc-exec"}},
		"network": map[string]any{
			"isolation": "namespace", "allow_from_namespaces": []any{"b", "a", "b", "t1"},
		},
	})
	must.NoError(t, err)
	must.NoError(t, spec.validate("t1"))
	d, err := spec.desired("t1")
	must.NoError(t, err)
	must.Eq(t, "t1", d.namespace.Name)
	must.Eq(t, "", d.namespace.Quota)
	must.Nil(t, d.quota)
	must.Eq(t, "t1-ops", d.policies[0].Name)
	must.Eq(t, "namespace \"t1\" {\n  capabilities = [\"read-job\", \"alloc-exec\"]\n}\n", d.policies[0].Rules)
	// Its own namespace, then the others once each, sorted.
	peers := d.network.Ingress[0].Peers
	must.Len(t, 3, peers)
	must.Eq(t, "", peers[0].Namespace)
	must.Eq(t, "a", peers[1].Namespace)
	must.Eq(t, "b", peers[2].Namespace)
}

// A quota of the tenant's name that is not the tenant's stops the pass: the
// namespace is not created without its quota (review finding TEN-4).
func TestTenant_QuotaConflict(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()

	var gen structs.GenericResponse
	must.NoError(t, c.s.RPC("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
		Quotas: []*structs.QuotaSpec{{Name: "team-q", Description: "made by hand", Limits: []*structs.QuotaLimit{
			{Region: "global", RegionLimit: &structs.Resources{CPU: 100}},
		}}},
		WriteRequest: c.write(""),
	}, &gen))

	_, err := c.apply("", structs.TenantKind, "team-q", map[string]any{
		"quota": map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 8000}}},
		"roles": map[string]any{"developer": []any{"submit-job"}},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-q", "conflict", func(o *structs.CustomObject) bool {
		return objectConditionTrue(o.Status, structs.TenantConditionConflict)
	})
	must.False(t, objectConditionTrue(obj.Status, structs.CertificateConditionReady))

	// The operator had its pass (the conflict is written at its end): the
	// namespace and the role must not be there.
	ns, err := store.NamespaceByName(nil, "team-q")
	must.NoError(t, err)
	must.Nil(t, ns, must.Sprint("the namespace was created without its quota"))
	role, err := store.GetACLRoleByName(nil, "team-q-developer")
	must.NoError(t, err)
	must.Nil(t, role)
	q, err := store.QuotaSpecByName(nil, "team-q")
	must.NoError(t, err)
	must.Eq(t, "made by hand", q.Description)
}

// A stored spec that is invalid keeps the namespace of the status: a later
// deletion waits for that namespace to empty (review finding TEN-9).
func TestTenant_InvalidSpecKeepsNamespace(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()

	_, err := c.apply("", structs.TenantKind, "keeps", map[string]any{})
	must.NoError(t, err)
	waitTenant(c, "keeps", "ready", tenantReady)

	// The write checks refuse an invalid spec, so it is stored past them.
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		changed := c.get("", structs.TenantKind, "keeps").Copy()
		changed.Spec = map[string]any{"roles": map[string]any{"dev": []any{"no-such-capability"}}}
		_, err := c.s.writeOperatorObject(changed)
		return err
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	obj := waitTenant(c, "keeps", "the invalid spec reported", func(o *structs.CustomObject) bool {
		return !objectConditionTrue(o.Status, structs.CertificateConditionReady)
	})
	must.Eq(t, "keeps", obj.Status["namespace"], must.Sprintf("status: %v", obj.Status))
	must.SliceContains(t, tenantResources(obj), "namespace//keeps")
}

// newTenantPass returns what a pass of the operator reads, for the tests
// that drive a tenant by hand.
func newTenantPass(t *testing.T, s *Server) *tenantPass {
	t.Helper()
	snap, err := s.State().Snapshot()
	must.NoError(t, err)
	p := &tenantPass{now: time.Now().UTC(), store: s.State(), ws: memdb.NewWatchSet()}
	p.releases, err = releaseOwners(snap)
	must.NoError(t, err)
	p.tenants, err = tenantOwners(snap)
	must.NoError(t, err)
	return p
}

// A resource one tenant took in a pass is not taken by another tenant of the
// same pass, and of two tenants that both list a resource one gives it up
// (review finding TEN-5).
func TestTenant_TakeOnce(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	o := newTenantOperator(c.s, c.s.logger)
	p := newTenantPass(t, c.s)

	tenant := func(name string, adopt ...string) *tenantWork {
		obj := &structs.CustomObject{Group: structs.OperatorsGroup, Kind: structs.TenantKind, Name: name}
		return &tenantWork{o: o, p: p, obj: obj, spec: &tenantSpec{Adopt: adopt}, owned: map[string]tenantResourceRef{}}
	}
	ref := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "x"}

	a := tenant("a")
	must.True(t, a.take(ref, false))

	// b comes later in the same pass: a has created x by now, and b adopts.
	b := tenant("b", structs.TenantResourceNamespace)
	must.False(t, b.take(ref, true), must.Sprint("b took the namespace a took in the same pass"))
	must.False(t, b.owns(ref))
	must.SliceLen(t, 1, b.conflicts)
	must.StrContains(t, b.conflicts[0], "belongs to the tenant a")

	// Two tenants that list the resource already: the pass knows one owner,
	// the other one gives the resource up.
	p.tenants[ref.key()] = "a"
	b2 := tenant("b")
	b2.owned[ref.key()] = ref
	must.False(t, b2.take(ref, true))
	must.False(t, b2.owns(ref), must.Sprint("b still lists the namespace of a"))
}

// What the tenant does not set on its quota and its ACL policies stays when
// the tenant writes them (review finding TEN-6).
func TestTenant_KeepsUnmanagedFields(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()

	spec := func(cpu int, caps ...any) map[string]any {
		return map[string]any{
			"quota": map[string]any{"region": map[string]any{"global": map[string]any{"cpu": cpu}}},
			"roles": map[string]any{"developer": caps},
		}
	}
	_, err := c.apply("", structs.TenantKind, "team-k", spec(8000, "submit-job"))
	must.NoError(t, err)
	waitTenant(c, "team-k", "ready", tenantReady)

	// An operator sets what the tenant has no field for.
	q, err := store.QuotaSpecByName(nil, "team-k")
	must.NoError(t, err)
	q = q.Copy()
	q.TaskMax = &structs.Resources{CPU: 4000}
	q.TaskDefaults = &structs.Resources{MemoryMB: 64}
	var gen structs.GenericResponse
	must.NoError(t, c.s.RPC("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
		Quotas: []*structs.QuotaSpec{q}, WriteRequest: c.write(""),
	}, &gen))
	pol, err := store.ACLPolicyByName(nil, "team-k-developer")
	must.NoError(t, err)
	changed := *pol
	changed.JobACL = &structs.JobACL{Namespace: "team-k", JobID: "builder"}
	must.NoError(t, c.s.RPC("ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
		Policies: []*structs.ACLPolicy{&changed}, WriteRequest: c.write(""),
	}, &gen))

	// A change of the tenant writes both again.
	_, err = c.apply("", structs.TenantKind, "team-k", spec(9000, "submit-job", "read-logs"))
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		q, _ := store.QuotaSpecByName(nil, "team-k")
		pol, _ := store.ACLPolicyByName(nil, "team-k-developer")
		return q != nil && q.Limits[0].RegionLimit.CPU == 9000 && pol != nil && strings.Contains(pol.Rules, "read-logs")
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))

	q, _ = store.QuotaSpecByName(nil, "team-k")
	must.NotNil(t, q.TaskMax, must.Sprint("the task_max of the quota is gone"))
	must.Eq(t, 4000, q.TaskMax.CPU)
	must.NotNil(t, q.TaskDefaults, must.Sprint("the task_defaults of the quota are gone"))
	must.Eq(t, 64, q.TaskDefaults.MemoryMB)
	pol, _ = store.ACLPolicyByName(nil, "team-k-developer")
	must.NotNil(t, pol.JobACL, must.Sprint("the job ACL of the policy is gone"))
	must.Eq(t, "builder", pol.JobACL.JobID)
}

// A deleted tenant waits for everything the deletion of a namespace refuses,
// before it deletes anything (review finding TEN-3).
func TestTenant_DeletionWaitsForVariablesAndObjects(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	s := c.s
	store := s.State()
	nextIndex := func() uint64 {
		index, err := store.LatestIndex()
		must.NoError(t, err)
		return index + 1
	}

	_, err := c.apply("", structs.TenantKind, "team-v", map[string]any{
		"roles":   map[string]any{"developer": []any{"submit-job"}},
		"network": map[string]any{"isolation": "namespace"},
	})
	must.NoError(t, err)
	waitTenant(c, "team-v", "ready", tenantReady)

	variable := mock.VariableEncrypted()
	variable.Namespace = "team-v"
	must.NoError(t, store.VarSet(nextIndex(), &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: variable}).Error)

	must.NoError(t, c.delete("", structs.TenantKind, "team-v"))
	message := func(o *structs.CustomObject) string {
		for _, cond := range operatorConditions(o.Status) {
			if cond.Type == structs.TenantConditionNamespaceNotEmpty && cond.Status == "True" {
				return cond.Message
			}
		}
		return ""
	}
	waitTenant(c, "team-v", "waiting for the variable", func(o *structs.CustomObject) bool {
		return strings.Contains(message(o), "1 variable(s)")
	})
	intact := func() {
		t.Helper()
		np, err := store.NetworkPolicyByName(nil, "team-v", structs.TenantNetworkPolicyName)
		must.NoError(t, err)
		must.NotNil(t, np, must.Sprint("the isolation went before the namespace was empty"))
		role, err := store.GetACLRoleByName(nil, "team-v-developer")
		must.NoError(t, err)
		must.NotNil(t, role, must.Sprint("the role went before the namespace was empty"))
	}
	intact()

	// An object of a namespaced kind holds the deletion back too.
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, nextIndex(), &structs.CustomObject{
		ID: uuid.Generate(), Group: structs.OperatorsGroup, Kind: structs.CertificateKind,
		Namespace: "team-v", Name: "left",
	}, 0))
	must.NoError(t, store.VarDelete(nextIndex(), &structs.VarApplyStateRequest{Op: structs.VarOpDelete, Var: variable}).Error)
	waitTenant(c, "team-v", "waiting for the object", func(o *structs.CustomObject) bool {
		m := message(o)
		return strings.Contains(m, "1 object(s)") && !strings.Contains(m, "variable")
	})
	intact()

	must.NoError(t, store.DeleteCustomObject(structs.MsgTypeTestSetup, nextIndex(),
		structs.OperatorsGroup, structs.CertificateKind, "team-v", "left", 0))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("", structs.TenantKind, "team-v") == nil
	}), wait.Timeout(60*time.Second), wait.Gap(100*time.Millisecond)))
	ns, _ := store.NamespaceByName(nil, "team-v")
	must.Nil(t, ns)
	role, _ := store.GetACLRoleByName(nil, "team-v-developer")
	must.Nil(t, role)
}

// The tenant writes what it is about to create into its status first, and
// creates nothing when that write fails: a status write lost later can't make
// it forget what it created (review finding TEN-1).
func TestTenant_IntentBeforeCreate(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	o := newTenantOperator(c.s, c.s.logger)

	// An object the store does not have: every write of its status fails as
	// a write on a stale modify index does, and the loop of the operators
	// never sees it.
	ghost := &structs.CustomObject{
		ID: uuid.Generate(), Group: structs.OperatorsGroup, Kind: structs.TenantKind, Name: "ghost",
		Generation: 1, ModifyIndex: 7, Finalizers: []string{structs.OperatorsFinalizer},
		Spec: map[string]any{
			"quota": map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 1000}}},
			"roles": map[string]any{"developer": []any{"submit-job"}},
		},
	}
	o.reconcileTenant(newTenantPass(t, c.s), ghost)

	ns, err := store.NamespaceByName(nil, "ghost")
	must.NoError(t, err)
	must.Nil(t, ns, must.Sprint("the namespace was created although the status could not be written"))
	q, err := store.QuotaSpecByName(nil, "ghost")
	must.NoError(t, err)
	must.Nil(t, q, must.Sprint("the quota was created although the status could not be written"))
	pol, err := store.ACLPolicyByName(nil, "ghost-developer")
	must.NoError(t, err)
	must.Nil(t, pol)
}

// What the status lists and the tenant could not create is marked pending,
// and nothing that exists is (review findings TEN-7 and TEN-1).
func TestTenant_PendingResources(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()

	// The quota can't be written: a limit without a region.
	spec := map[string]any{
		"quota": map[string]any{"region": map[string]any{"": map[string]any{"cpu": 1000}}},
	}
	_, err := c.apply("", structs.TenantKind, "team-p", spec)
	must.NoError(t, err)
	obj := waitTenant(c, "team-p", "the failed write reported", func(o *structs.CustomObject) bool {
		return o.Status["observedGeneration"] != nil && !objectConditionTrue(o.Status, structs.CertificateConditionReady)
	})
	must.SliceNotEmpty(t, obj.Status["resources"].([]any))
	for _, raw := range obj.Status["resources"].([]any) {
		ref := raw.(map[string]any)
		must.Eq(t, true, ref["pending"], must.Sprintf("%v was never created and is not marked pending", ref))
	}
	q, _ := store.QuotaSpecByName(nil, "team-p")
	must.Nil(t, q)
	ns, _ := store.NamespaceByName(nil, "team-p")
	must.Nil(t, ns)

	// With a quota that can be written everything is created and nothing
	// is pending.
	spec["quota"] = map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 1000}}}
	_, err = c.apply("", structs.TenantKind, "team-p", spec)
	must.NoError(t, err)
	obj = waitTenant(c, "team-p", "ready", tenantReady)
	must.SliceLen(t, 2, obj.Status["resources"].([]any))
	for _, raw := range obj.Status["resources"].([]any) {
		ref := raw.(map[string]any)
		must.MapNotContainsKey(t, ref, "pending", must.Sprintf("%v exists and is marked pending", ref))
	}
}

// A role grants rights on objects of kinds in the namespace of the tenant:
// the object blocks of its namespace rule (review finding PLD-2).
func TestTenantSpec_RoleObjects(t *testing.T) {
	ci.Parallel(t)
	spec, err := decodeTenantSpec("t1", map[string]any{
		"roles": map[string]any{
			"dev": map[string]any{
				"capabilities": []any{"submit-job"},
				"objects": map[string]any{
					"norca.io/LoadBalancer": []any{"read", "write"},
					"norca.io/DNSRecord":    []any{"read"},
				},
			},
			"objects-only": map[string]any{
				"objects": map[string]any{"example.com/*": []any{"read"}},
			},
		},
	})
	must.NoError(t, err)
	must.NoError(t, spec.validate("t1"))
	d, err := spec.desired("t1")
	must.NoError(t, err)
	must.Eq(t, "t1-dev", d.policies[0].Name)
	must.Eq(t, `namespace "t1" {
  capabilities = ["submit-job"]
  object "norca.io/DNSRecord" {
    capabilities = ["read"]
  }
  object "norca.io/LoadBalancer" {
    capabilities = ["read", "write"]
  }
}
`, d.policies[0].Rules)

	// The policy grants the objects in the namespace of the tenant, and
	// nowhere else.
	parsed, err := acl.Parse(d.policies[0].Rules)
	must.NoError(t, err)
	aclObj, err := acl.NewACL(false, []*acl.Policy{parsed})
	must.NoError(t, err)
	must.True(t, aclObj.AllowObjectOperation("t1", "norca.io/LoadBalancer", acl.ObjectCapabilityWrite))
	must.True(t, aclObj.AllowObjectOperation("t1", "norca.io/DNSRecord", acl.ObjectCapabilityRead))
	must.False(t, aclObj.AllowObjectOperation("t1", "norca.io/DNSRecord", acl.ObjectCapabilityWrite))
	must.False(t, aclObj.AllowObjectOperation("other", "norca.io/LoadBalancer", acl.ObjectCapabilityRead))
	must.False(t, aclObj.AllowObjectOperation(acl.ObjectClusterNamespace, "norca.io/Tenant", acl.ObjectCapabilityRead))
	must.True(t, aclObj.AllowNsOp("t1", acl.NamespaceCapabilitySubmitJob))

	must.Eq(t, "t1-objects-only", d.policies[1].Name)
	must.Eq(t, `namespace "t1" {
  object "example.com/*" {
    capabilities = ["read"]
  }
}
`, d.policies[1].Rules)

	// What the ACL parser refuses is refused when the tenant is written.
	for name, objects := range map[string]map[string]any{
		"bad capability": {"norca.io/LoadBalancer": []any{"no-such-capability"}},
		"bad kind":       {"LoadBalancer": []any{"read"}},
		"quote in kind":  {"norca.io/X\" {}\nnode { policy = \"write\" }\n#": []any{"read"}},
		"no capability":  {"norca.io/LoadBalancer": []any{}},
		"not a list":     {"norca.io/LoadBalancer": "read"},
	} {
		spec, err := decodeTenantSpec("t1", map[string]any{
			"roles": map[string]any{"dev": map[string]any{"objects": objects}},
		})
		must.NoError(t, err)
		must.ErrorContains(t, spec.validate("t1"), `role "dev"`, must.Sprint(name))
	}
}

// A deleted tenant of a cluster with two regions and ACLs: the deletion of
// its namespace asks the other region for the jobs of the namespace, which
// the leader can do with the replication token only (review finding TEN-2).
func TestTenant_DeletionWithTwoRegions(t *testing.T) {
	ci.Parallel(t)

	// regions starts the authoritative region1 and region2, and returns
	// the first with its root token.
	regions := func(t *testing.T, replicationToken string) *certTest {
		s1, root, cleanupS1 := TestACLServer(t, func(c *Config) {
			c.Region = "region1"
			c.AuthoritativeRegion = "region1"
			c.ReplicationToken = replicationToken
		})
		t.Cleanup(cleanupS1)
		s2, _, cleanupS2 := TestACLServer(t, func(c *Config) {
			c.Region = "region2"
			c.AuthoritativeRegion = "region1"
			c.ReplicationBackoff = 20 * time.Millisecond
			c.ReplicationToken = root.SecretID
		})
		t.Cleanup(cleanupS2)
		TestJoin(t, s1, s2)
		testutil.WaitForLeader(t, s1.RPC)
		testutil.WaitForLeader(t, s2.RPC)
		waitTenantKind(t, s1)

		if replicationToken != "" {
			// The token the servers of region1 are configured with: a
			// global management token, which region2 replicates.
			token := mock.ACLManagementToken()
			token.SecretID = replicationToken
			must.NoError(t, s1.State().UpsertACLTokens(structs.MsgTypeTestSetup, 100, []*structs.ACLToken{token}))
			must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
				out, err := s2.State().ACLTokenBySecretID(nil, replicationToken)
				return err == nil && out != nil
			}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the token is replicated to region2"))
		}
		return &certTest{t: t, s: s1, token: root.SecretID}
	}
	write := func(c *certTest) structs.WriteRequest {
		return structs.WriteRequest{Region: "region1", AuthToken: c.token}
	}
	apply := func(c *certTest, name string) {
		var resp structs.CustomObjectWriteResponse
		must.NoError(t, c.s.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
			Object:       &structs.CustomObject{Group: structs.OperatorsGroup, Kind: structs.TenantKind, Name: name},
			WriteRequest: write(c),
		}, &resp))
		waitTenant(c, name, "ready", tenantReady)
	}
	remove := func(c *certTest, name string) {
		var resp structs.CustomObjectWriteResponse
		must.NoError(t, c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
			Group: structs.OperatorsGroup, Kind: structs.TenantKind, Name: name, WriteRequest: write(c),
		}, &resp))
	}

	t.Run("with the replication token", func(t *testing.T) {
		c := regions(t, uuid.Generate())
		apply(c, "fed")
		remove(c, "fed")
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			return c.get("", structs.TenantKind, "fed") == nil
		}), wait.Timeout(60*time.Second), wait.Gap(100*time.Millisecond)),
			must.Func(func() string {
				log, _ := c.s.State().ObjectEventLog(nil, structs.OperatorsGroup, structs.TenantKind, "", "fed")
				var events []string
				if log != nil {
					for _, e := range log.Events {
						events = append(events, e.Reason+": "+e.Message)
					}
				}
				return fmt.Sprintf("the tenant is still there; events: %q", events)
			}))
		ns, err := c.s.State().NamespaceByName(nil, "fed")
		must.NoError(t, err)
		must.Nil(t, ns)
	})

	t.Run("without it the event says what to configure", func(t *testing.T) {
		c := regions(t, "")
		apply(c, "fed")
		remove(c, "fed")
		var message string
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			log, err := c.s.State().ObjectEventLog(nil, structs.OperatorsGroup, structs.TenantKind, "", "fed")
			if err != nil || log == nil {
				return false
			}
			for _, e := range log.Events {
				if e.Reason == "DeleteFailed" {
					message = e.Message
					return true
				}
			}
			return false
		}), wait.Timeout(60*time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("a DeleteFailed event"))
		must.StrContains(t, message, "replication_token")
		must.StrContains(t, message, `region "region2"`)
		ns, err := c.s.State().NamespaceByName(nil, "fed")
		must.NoError(t, err)
		must.NotNil(t, ns)
	})

	// Who is not the leader keeps its own token: the replication token is
	// not lent to a caller the other region refuses.
	t.Run("a caller keeps its own token", func(t *testing.T) {
		c := regions(t, uuid.Generate())
		must.NoError(t, c.s.State().UpsertNamespaces(200, []*structs.Namespace{{Name: "plain"}}))
		// A management token of region1 only: region2 does not know it.
		local := mock.ACLManagementToken()
		local.Global = false
		must.NoError(t, c.s.State().UpsertACLTokens(structs.MsgTypeTestSetup, 201, []*structs.ACLToken{local}))
		var resp structs.GenericResponse
		err := c.s.RPC("Namespace.DeleteNamespaces", &structs.NamespaceDeleteRequest{
			Namespaces:   []string{"plain"},
			WriteRequest: structs.WriteRequest{Region: "region1", AuthToken: local.SecretID},
		}, &resp)
		must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
		must.StrNotContains(t, err.Error(), "replication_token")
	})
}
