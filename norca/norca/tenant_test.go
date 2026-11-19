// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"net/http"
	"sort"
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

// waitTenantKind waits for the kind Tenant, with the other kinds of the
// built-in operators: see waitOperatorKinds.
func waitTenantKind(t *testing.T, s *Server) {
	t.Helper()
	waitOperatorKinds(t, s)
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
		{"cidrs without isolation", map[string]any{"network": map[string]any{
			"allow_from_cidrs": []any{"10.0.0.0/24"}}}, "need isolation"},
		{"required without isolation", map[string]any{"network": map[string]any{
			"required": false}}, "need isolation"},
		{"bad cidr", map[string]any{"network": map[string]any{
			"isolation": "namespace", "allow_from_cidrs": []any{"10.0.0.1"}}}, "allow_from_cidrs"},
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
	must.True(t, a.take(ref, nil, &tenantOriginal{}))

	// b comes later in the same pass: a has created x by now, and b adopts.
	b := tenant("b", structs.TenantResourceNamespace)
	must.False(t, b.take(ref, &tenantOriginal{}, &tenantOriginal{}), must.Sprint("b took the namespace a took in the same pass"))
	must.False(t, b.owns(ref))
	must.SliceLen(t, 1, b.conflicts)
	must.StrContains(t, b.conflicts[0], "belongs to the tenant a")

	// Two tenants that list the resource already: the pass knows one owner,
	// the other one gives the resource up.
	p.tenants[ref.key()] = "a"
	b2 := tenant("b")
	b2.owned[ref.key()] = ref
	must.False(t, b2.take(ref, &tenantOriginal{}, &tenantOriginal{}))
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

// tenantByHand makes the resources a tenant is to take over, as an operator
// of the cluster would by hand.
type tenantByHand struct {
	c *certTest
}

func (h tenantByHand) namespace(name, quota string, meta map[string]string) {
	h.c.t.Helper()
	var resp structs.GenericResponse
	must.NoError(h.c.t, h.c.s.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces:   []*structs.Namespace{{Name: name, Description: "made by hand", Quota: quota, Meta: meta}},
		WriteRequest: h.c.write(""),
	}, &resp))
}

func (h tenantByHand) quota(name string) {
	h.c.t.Helper()
	var resp structs.GenericResponse
	must.NoError(h.c.t, h.c.s.RPC("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
		Quotas: []*structs.QuotaSpec{{Name: name, Description: "made by hand", Limits: []*structs.QuotaLimit{
			{Region: "global", RegionLimit: &structs.Resources{CPU: 100, MemoryMB: 200}},
		}}},
		WriteRequest: h.c.write(""),
	}, &resp))
}

func (h tenantByHand) deleteQuota(name string) {
	h.c.t.Helper()
	var resp structs.GenericResponse
	must.NoError(h.c.t, h.c.s.RPC("Quota.DeleteQuotaSpecs", &structs.QuotaSpecDeleteRequest{
		Names: []string{name}, WriteRequest: h.c.write(""),
	}, &resp))
}

func (h tenantByHand) policy(name, rules string) {
	h.c.t.Helper()
	var resp structs.GenericResponse
	must.NoError(h.c.t, h.c.s.RPC("ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
		Policies:     []*structs.ACLPolicy{{Name: name, Description: "made by hand", Rules: rules}},
		WriteRequest: h.c.write(""),
	}, &resp))
}

// policyAndRole makes a policy, a second policy "<name>-other", and a role
// of the name that links the second one: what a tenant never writes.
func (h tenantByHand) policyAndRole(name string) {
	h.c.t.Helper()
	h.policy(name, `node { policy = "read" }`)
	h.policy(name+"-other", `agent { policy = "read" }`)
	var roles structs.ACLRolesUpsertResponse
	must.NoError(h.c.t, h.c.s.RPC(structs.ACLUpsertRolesRPCMethod, &structs.ACLRolesUpsertRequest{
		ACLRoles: []*structs.ACLRole{{Name: name, Description: "made by hand",
			Policies: []*structs.ACLRolePolicyLink{{Name: name + "-other"}}}},
		WriteRequest: h.c.write(""),
	}, &roles))
}

func (h tenantByHand) networkPolicy(namespace string) *structs.NetworkPolicy {
	h.c.t.Helper()
	np := &structs.NetworkPolicy{
		Name: structs.TenantNetworkPolicyName, Namespace: namespace, Description: "made by hand",
		Ingress: []*structs.NetworkPolicyRule{{Name: "office", Peers: []*structs.NetworkPolicyPeer{{CIDR: []string{"192.0.2.0/24"}}}}},
	}
	var resp structs.GenericResponse
	must.NoError(h.c.t, h.c.s.RPC("NetworkPolicy.UpsertNetworkPolicies", &structs.NetworkPolicyUpsertRequest{
		NetworkPolicies: []*structs.NetworkPolicy{np.Copy()}, WriteRequest: h.c.write(namespace),
	}, &resp))
	return np
}

// tenantRefs returns the resources of the status of a tenant by key.
func tenantRefs(obj *structs.CustomObject) map[string]tenantResourceRef {
	st := &tenantStatus{}
	_ = structs.DecodeObjectPart(obj.Status, st)
	out := map[string]tenantResourceRef{}
	for _, r := range st.Resources {
		out[r.key()] = r
	}
	return out
}

// tenantAdopted returns the keys of the resources the status marks adopted.
func tenantAdopted(obj *structs.CustomObject) []string {
	out := []string{}
	for key, r := range tenantRefs(obj) {
		if r.Adopted {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// tenantEvents returns the events of a tenant as "Reason: message".
func tenantEvents(c *certTest, name string) string {
	log, err := c.s.State().ObjectEventLog(nil, structs.OperatorsGroup, structs.TenantKind, "", name)
	must.NoError(c.t, err)
	var out []string
	if log != nil {
		for _, e := range log.Events {
			out = append(out, e.Reason+": "+e.Message)
		}
	}
	return strings.Join(out, "\n")
}

func waitTenantGone(c *certTest, name string) {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("", structs.TenantKind, name) == nil
	}), wait.Timeout(60*time.Second), wait.Gap(100*time.Millisecond)),
		must.Func(func() string {
			return fmt.Sprintf("the tenant %s is still there: %v; events: %s", name, c.get("", structs.TenantKind, name), tenantEvents(c, name))
		}))
}

// What a tenant took over is given back as the tenant found it when the
// tenant is deleted; what it created goes (review finding TEN-8, adoption as
// a loan).
func TestTenant_AdoptedGivenBack(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	hand := tenantByHand{c}

	hand.quota("old-quota")
	hand.namespace("kept", "old-quota", map[string]string{"team": "old"})
	hand.quota("team-ad")
	hand.policyAndRole("team-ad-dev")
	handNP := hand.networkPolicy("kept")

	_, err := c.apply("", structs.TenantKind, "team-ad", map[string]any{
		"namespace": map[string]any{"name": "kept", "description": "Team AD", "meta": map[string]any{"owner": "ad"}},
		"quota":     map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 8000}}},
		"roles": map[string]any{
			"dev":   []any{"submit-job"},
			"extra": []any{"read-job"},
		},
		"network":      map[string]any{"isolation": "namespace"},
		"dns_suffixes": []any{"ad.example.com"},
		"adopt":        []any{"namespace", "quota", "acl-policy", "acl-role", "network-policy"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-ad", "ready", tenantReady)
	must.Eq(t, []string{
		"acl-policy//team-ad-dev", "acl-role//team-ad-dev", "namespace//kept",
		"network-policy/kept/tenant-isolation", "quota//team-ad",
	}, tenantAdopted(obj), must.Sprintf("status: %v", obj.Status))
	refs := tenantRefs(obj)
	must.Eq(t, &tenantOriginal{Description: "made by hand", Meta: map[string]string{"team": "old"}, Quota: "old-quota"},
		refs["namespace//kept"].Original)
	must.Eq(t, `node { policy = "read" }`, refs["acl-policy//team-ad-dev"].Original.Rules)
	must.Eq(t, []string{"team-ad-dev-other"}, refs["acl-role//team-ad-dev"].Original.Policies)
	must.Nil(t, refs["acl-policy//team-ad-extra"].Original)

	// While the tenant lives, what it took over is as its spec says.
	ns, _ := store.NamespaceByName(nil, "kept")
	must.Eq(t, "Team AD", ns.Description)
	must.Eq(t, "team-ad", ns.Quota)
	must.Eq(t, "ad.example.com", ns.Meta[structs.NamespaceMetaDNSSuffixes])
	q, _ := store.QuotaSpecByName(nil, "team-ad")
	must.Eq(t, 8000, q.Limits[0].RegionLimit.CPU)
	pol, _ := store.ACLPolicyByName(nil, "team-ad-dev")
	must.StrContains(t, pol.Rules, "submit-job")
	role, _ := store.GetACLRoleByName(nil, "team-ad-dev")
	must.Eq(t, "team-ad-dev", role.Policies[0].Name)
	np, _ := store.NetworkPolicyByName(nil, "kept", structs.TenantNetworkPolicyName)
	must.Eq(t, "tenant", np.Ingress[0].Name)

	// A variable in the namespace: the tenant does not delete the namespace,
	// so it does not wait for it to empty.
	variable := mock.VariableEncrypted()
	variable.Namespace = "kept"
	index, err := store.LatestIndex()
	must.NoError(t, err)
	must.NoError(t, store.VarSet(index+1, &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: variable}).Error)

	must.NoError(t, c.delete("", structs.TenantKind, "team-ad"))
	waitTenantGone(c, "team-ad")

	// Everything it took over is as it was.
	ns, _ = store.NamespaceByName(nil, "kept")
	must.NotNil(t, ns)
	must.Eq(t, "made by hand", ns.Description)
	must.Eq(t, map[string]string{"team": "old"}, ns.Meta)
	must.Eq(t, "old-quota", ns.Quota)
	q, _ = store.QuotaSpecByName(nil, "team-ad")
	must.NotNil(t, q)
	must.Eq(t, "made by hand", q.Description)
	must.Len(t, 1, q.Limits)
	must.Eq(t, 100, q.Limits[0].RegionLimit.CPU)
	must.Eq(t, 200, q.Limits[0].RegionLimit.MemoryMB)
	pol, _ = store.ACLPolicyByName(nil, "team-ad-dev")
	must.NotNil(t, pol)
	must.Eq(t, `node { policy = "read" }`, pol.Rules)
	must.Eq(t, "made by hand", pol.Description)
	role, _ = store.GetACLRoleByName(nil, "team-ad-dev")
	must.NotNil(t, role)
	must.Eq(t, "made by hand", role.Description)
	must.Len(t, 1, role.Policies)
	must.Eq(t, "team-ad-dev-other", role.Policies[0].Name)
	np, _ = store.NetworkPolicyByName(nil, "kept", structs.TenantNetworkPolicyName)
	must.NotNil(t, np)
	must.True(t, networkPolicyMatches(np, handNP), must.Sprintf("network policy: %#v", np))

	// What it created is gone.
	pol, _ = store.ACLPolicyByName(nil, "team-ad-extra")
	must.Nil(t, pol)
	role, _ = store.GetACLRoleByName(nil, "team-ad-extra")
	must.Nil(t, role)
}

// A namespace the tenant created goes, the quota it took over is given back;
// the event of the deletion says so while the tenant waits for the
// namespace.
func TestTenant_AdoptedQuotaCreatedNamespace(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	tenantByHand{c}.quota("team-bq")

	_, err := c.apply("", structs.TenantKind, "team-bq", map[string]any{
		"quota": map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 8000}}},
		"adopt": []any{"quota"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-bq", "ready", tenantReady)
	must.Eq(t, []string{"quota//team-bq"}, tenantAdopted(obj))

	// A job holds the deletion back: the namespace is the tenant's own.
	job := mock.Job()
	job.Namespace = "team-bq"
	var jobResp structs.JobRegisterResponse
	must.NoError(t, c.s.RPC("Job.Register", &structs.JobRegisterRequest{Job: job, WriteRequest: c.write("team-bq")}, &jobResp))
	must.NoError(t, c.delete("", structs.TenantKind, "team-bq"))
	waitTenant(c, "team-bq", "waiting for the namespace", func(o *structs.CustomObject) bool {
		return objectConditionTrue(o.Status, structs.TenantConditionNamespaceNotEmpty)
	})
	must.StrContains(t, tenantEvents(c, "team-bq"),
		"Released: the deleted tenant gives back what it took over, restored to what the tenant found: quota team-bq")

	var deregResp structs.JobDeregisterResponse
	must.NoError(t, c.s.RPC("Job.Deregister", &structs.JobDeregisterRequest{
		JobID: job.ID, Purge: true, WriteRequest: c.write("team-bq"),
	}, &deregResp))
	waitTenantGone(c, "team-bq")
	ns, _ := store.NamespaceByName(nil, "team-bq")
	must.Nil(t, ns)
	q, _ := store.QuotaSpecByName(nil, "team-bq")
	must.NotNil(t, q)
	must.Eq(t, 100, q.Limits[0].RegionLimit.CPU)
}

// A namespace the tenant took over names its own quota again when the
// tenant goes, and the quota the tenant created for it is deleted.
func TestTenant_AdoptedNamespaceCreatedQuota(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	hand := tenantByHand{c}
	hand.quota("old-quota")
	hand.namespace("team-cq", "old-quota", nil)

	_, err := c.apply("", structs.TenantKind, "team-cq", map[string]any{
		"quota": map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 8000}}},
		"roles": map[string]any{"dev": []any{"submit-job"}},
		"adopt": []any{"namespace"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-cq", "ready", tenantReady)
	must.Eq(t, []string{"namespace//team-cq"}, tenantAdopted(obj))
	ns, _ := store.NamespaceByName(nil, "team-cq")
	must.Eq(t, "team-cq", ns.Quota)

	must.NoError(t, c.delete("", structs.TenantKind, "team-cq"))
	waitTenantGone(c, "team-cq")
	ns, _ = store.NamespaceByName(nil, "team-cq")
	must.NotNil(t, ns)
	must.Eq(t, "old-quota", ns.Quota)
	q, _ := store.QuotaSpecByName(nil, "team-cq")
	must.Nil(t, q, must.Sprint("the quota the tenant created stayed"))
	role, _ := store.GetACLRoleByName(nil, "team-cq-dev")
	must.Nil(t, role)
}

// A role the tenant took over and that leaves the spec is given back as it
// was; one the tenant created is deleted.
func TestTenant_AdoptedRoleLeavesSpec(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	tenantByHand{c}.policyAndRole("team-rl-old")

	_, err := c.apply("", structs.TenantKind, "team-rl", map[string]any{
		"roles": map[string]any{"old": []any{"read-job"}, "new": []any{"submit-job"}},
		"adopt": []any{"acl-policy", "acl-role"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-rl", "ready", tenantReady)
	must.Eq(t, []string{"acl-policy//team-rl-old", "acl-role//team-rl-old"}, tenantAdopted(obj))
	role, _ := store.GetACLRoleByName(nil, "team-rl-old")
	must.Eq(t, "team-rl-old", role.Policies[0].Name)

	// Both roles leave the spec, and adopt with them: the mark stays.
	_, err = c.apply("", structs.TenantKind, "team-rl", map[string]any{})
	must.NoError(t, err)
	obj = waitTenant(c, "team-rl", "the roles gone from the status", func(o *structs.CustomObject) bool {
		return tenantReady(o) && len(tenantResources(o)) == 1
	})
	must.Eq(t, []string{"namespace//team-rl"}, tenantResources(obj))

	pol, _ := store.ACLPolicyByName(nil, "team-rl-old")
	must.NotNil(t, pol)
	must.Eq(t, `node { policy = "read" }`, pol.Rules)
	role, _ = store.GetACLRoleByName(nil, "team-rl-old")
	must.NotNil(t, role)
	must.Eq(t, "made by hand", role.Description)
	must.Eq(t, "team-rl-old-other", role.Policies[0].Name)
	pol, _ = store.ACLPolicyByName(nil, "team-rl-new")
	must.Nil(t, pol)
	role, _ = store.GetACLRoleByName(nil, "team-rl-new")
	must.Nil(t, role)

	events := tenantEvents(c, "team-rl")
	must.StrContains(t, events, "Restored: gave acl-role team-rl-old back as the tenant found it")
	must.StrContains(t, events, "Restored: gave acl-policy team-rl-old back as the tenant found it")
	must.StrContains(t, events, "Deleted: deleted acl-role team-rl-new")
}

// A resource that can't be given back as it was keeps the tenant, with the
// condition RestoreFailed, until release_without_restore lets it go as it
// is.
func TestTenant_RestoreBlocked(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	hand := tenantByHand{c}
	hand.quota("orig-q")
	hand.namespace("team-rb", "orig-q", nil)

	spec := map[string]any{
		"namespace": map[string]any{"description": "Team RB"},
		"quota":     map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 8000}}},
		"adopt":     []any{"namespace"},
	}
	_, err := c.apply("", structs.TenantKind, "team-rb", spec)
	must.NoError(t, err)
	waitTenant(c, "team-rb", "ready", tenantReady)

	// The quota the namespace named goes meanwhile.
	hand.deleteQuota("orig-q")

	must.NoError(t, c.delete("", structs.TenantKind, "team-rb"))
	obj := waitTenant(c, "team-rb", "the restore reported", func(o *structs.CustomObject) bool {
		return objectConditionTrue(o.Status, structs.TenantConditionRestoreFailed)
	})
	var message string
	for _, cond := range operatorConditions(obj.Status) {
		if cond.Type == structs.TenantConditionRestoreFailed {
			message = cond.Message
		}
	}
	must.StrContains(t, message, `namespace team-rb named the quota "orig-q" when the tenant took it over, which no longer exists`)
	must.StrContains(t, message, "release_without_restore")
	must.StrContains(t, tenantEvents(c, "team-rb"), "RestoreFailed: giving namespace team-rb back failed")
	// Nothing was given up meanwhile.
	ns, _ := store.NamespaceByName(nil, "team-rb")
	must.Eq(t, "Team RB", ns.Description)
	must.Eq(t, "team-rb", ns.Quota)
	must.SliceContains(t, tenantAdopted(obj), "namespace//team-rb")

	// The way out, written into the tenant that is being deleted.
	spec["release_without_restore"] = true
	_, err = c.apply("", structs.TenantKind, "team-rb", spec)
	must.NoError(t, err)
	waitTenantGone(c, "team-rb")
	ns, _ = store.NamespaceByName(nil, "team-rb")
	must.NotNil(t, ns)
	must.Eq(t, "Team RB", ns.Description)
	// As it is: it still names the quota of the tenant, which stays.
	must.Eq(t, "team-rb", ns.Quota)
	q, _ := store.QuotaSpecByName(nil, "team-rb")
	must.NotNil(t, q)
}

// What the tenant took over, was deleted by hand and the tenant made again
// is the tenant's own: it goes with the tenant.
func TestTenant_AdoptedRecreated(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	tenantByHand{c}.policyAndRole("team-rc-dev")

	_, err := c.apply("", structs.TenantKind, "team-rc", map[string]any{
		"roles": map[string]any{"dev": []any{"read-job"}},
		"adopt": []any{"acl-policy", "acl-role"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-rc", "ready", tenantReady)
	must.Eq(t, []string{"acl-policy//team-rc-dev", "acl-role//team-rc-dev"}, tenantAdopted(obj))

	var resp structs.GenericResponse
	must.NoError(t, c.s.RPC("ACL.DeletePolicies", &structs.ACLPolicyDeleteRequest{
		Names: []string{"team-rc-dev"}, WriteRequest: c.write(""),
	}, &resp))
	obj = waitTenant(c, "team-rc", "the policy made again as the tenant's own", func(o *structs.CustomObject) bool {
		ref, ok := tenantRefs(o)["acl-policy//team-rc-dev"]
		return tenantReady(o) && ok && !ref.Adopted && !ref.Pending && ref.Original == nil
	})
	must.Eq(t, []string{"acl-role//team-rc-dev"}, tenantAdopted(obj))
	pol, _ := store.ACLPolicyByName(nil, "team-rc-dev")
	must.NotNil(t, pol)

	must.NoError(t, c.delete("", structs.TenantKind, "team-rc"))
	waitTenantGone(c, "team-rc")
	pol, _ = store.ACLPolicyByName(nil, "team-rc-dev")
	must.Nil(t, pol, must.Sprint("the policy the tenant made again stayed"))
	role, _ := store.GetACLRoleByName(nil, "team-rc-dev")
	must.NotNil(t, role)
	must.Eq(t, "team-rc-dev-other", role.Policies[0].Name)
}

// A resource whose original does not fit into the status is not taken over.
func TestTenant_OversizeOriginalRefused(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()

	rules := `node { policy = "read" }` + "\n" + strings.Repeat("# "+strings.Repeat("x", 98)+"\n", 400)
	must.Greater(t, tenantOriginalMax, len(rules))
	tenantByHand{c}.policy("team-big-dev", rules)

	_, err := c.apply("", structs.TenantKind, "team-big", map[string]any{
		"roles": map[string]any{"dev": []any{"read-job"}},
		"adopt": []any{"acl-policy", "acl-role"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-big", "conflict", func(o *structs.CustomObject) bool {
		return objectConditionTrue(o.Status, structs.TenantConditionConflict)
	})
	var message string
	for _, cond := range operatorConditions(obj.Status) {
		if cond.Type == structs.CertificateConditionReady {
			message = cond.Message
		}
	}
	must.StrContains(t, message, "acl-policy team-big-dev can't be taken over")
	must.SliceNotContains(t, tenantResources(obj), "acl-policy//team-big-dev")
	pol, _ := store.ACLPolicyByName(nil, "team-big-dev")
	must.Eq(t, rules, pol.Rules)
}

// A tenant without a quota block leaves the quota its namespace names
// alone, in a namespace it created and in one it took over.
func TestTenant_NoQuotaBlockLeavesReference(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	hand := tenantByHand{c}
	hand.quota("hand-q")

	// Created by the tenant, then given a quota by hand.
	_, err := c.apply("", structs.TenantKind, "team-nq", map[string]any{"namespace": map[string]any{"description": "Team NQ"}})
	must.NoError(t, err)
	waitTenant(c, "team-nq", "ready", tenantReady)
	ns, _ := store.NamespaceByName(nil, "team-nq")
	must.Eq(t, "", ns.Quota)
	changed := ns.Copy()
	changed.Quota = "hand-q"
	changed.Description = "changed by hand"
	var resp structs.GenericResponse
	must.NoError(t, c.s.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces: []*structs.Namespace{changed}, WriteRequest: c.write(""),
	}, &resp))
	// The description is set back (the pass ran), the quota is not.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		ns, _ := store.NamespaceByName(nil, "team-nq")
		return ns != nil && ns.Description == "Team NQ"
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))
	ns, _ = store.NamespaceByName(nil, "team-nq")
	must.Eq(t, "hand-q", ns.Quota, must.Sprint("the tenant without a quota took the quota off its namespace"))

	// Taken over with the quota it names.
	hand.namespace("taken-nq", "hand-q", nil)
	_, err = c.apply("", structs.TenantKind, "team-nq2", map[string]any{
		"namespace": map[string]any{"name": "taken-nq", "description": "Team NQ2"},
		"adopt":     []any{"namespace"},
	})
	must.NoError(t, err)
	waitTenant(c, "team-nq2", "ready", tenantReady)
	ns, _ = store.NamespaceByName(nil, "taken-nq")
	must.Eq(t, "Team NQ2", ns.Description)
	must.Eq(t, "hand-q", ns.Quota, must.Sprint("the tenant without a quota took the quota off the namespace it took over"))
}

// A namespace goes from one tenant to another in two steps: the first
// tenant gives it back, the second one takes over what it finds.
func TestTenant_HandOver(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	tenantByHand{c}.namespace("shared", "", map[string]string{"team": "old"})

	spec := func(description string) map[string]any {
		return map[string]any{
			"namespace":    map[string]any{"name": "shared", "description": description},
			"dns_suffixes": []any{"shared.example.com"},
			"adopt":        []any{"namespace"},
		}
	}
	_, err := c.apply("", structs.TenantKind, "first", spec("Of the first"))
	must.NoError(t, err)
	waitTenant(c, "first", "ready", tenantReady)

	// While the first tenant has it the second one can't.
	_, err = c.apply("", structs.TenantKind, "second", spec("Of the second"))
	must.ErrorContains(t, err, `belongs to the tenant "first"`)

	must.NoError(t, c.delete("", structs.TenantKind, "first"))
	waitTenantGone(c, "first")
	ns, _ := store.NamespaceByName(nil, "shared")
	must.Eq(t, "made by hand", ns.Description)
	must.Eq(t, map[string]string{"team": "old"}, ns.Meta)

	_, err = c.apply("", structs.TenantKind, "second", spec("Of the second"))
	must.NoError(t, err)
	obj := waitTenant(c, "second", "ready", tenantReady)
	must.Eq(t, &tenantOriginal{Description: "made by hand", Meta: map[string]string{"team": "old"}},
		tenantRefs(obj)["namespace//shared"].Original)
	ns, _ = store.NamespaceByName(nil, "shared")
	must.Eq(t, "Of the second", ns.Description)
}

// A resource the status lists as pending is a claim on the name, not
// ownership: one that turns up and is not what the tenant was creating is
// not the tenant's (review finding TEN-7, the remainder).
func TestTenant_PendingIsNotOwnership(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()

	// The quota can't be created (a limit without a region), so the pass
	// stops with the quota and the namespace listed as pending.
	spec := map[string]any{
		"namespace": map[string]any{"description": "Team P7"},
		"quota":     map[string]any{"region": map[string]any{"": map[string]any{"cpu": 1000}}},
	}
	_, err := c.apply("", structs.TenantKind, "team-p7", spec)
	must.NoError(t, err)
	obj := waitTenant(c, "team-p7", "the failed write reported", func(o *structs.CustomObject) bool {
		return o.Status["observedGeneration"] != nil && !objectConditionTrue(o.Status, structs.CertificateConditionReady)
	})
	must.True(t, tenantRefs(obj)["namespace//team-p7"].Pending)

	// Somebody makes a namespace of the name by hand meanwhile.
	tenantByHand{c}.namespace("team-p7", "", map[string]string{"team": "hand"})

	// The spec is repaired: the tenant can create its quota now, and finds
	// the namespace.
	spec["quota"] = map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 1000}}}
	_, err = c.apply("", structs.TenantKind, "team-p7", spec)
	must.NoError(t, err)
	obj = waitTenant(c, "team-p7", "the new spec seen", func(o *structs.CustomObject) bool {
		return toInt64(o.Status["observedGeneration"]) == int64(o.Generation)
	})
	must.True(t, objectConditionTrue(obj.Status, structs.TenantConditionConflict),
		must.Sprintf("the tenant took a namespace it did not create, without adopt; status: %v", obj.Status))
	ns, _ := store.NamespaceByName(nil, "team-p7")
	must.Eq(t, "made by hand", ns.Description)
	must.Eq(t, map[string]string{"team": "hand"}, ns.Meta)
	must.MapNotContainsKey(t, tenantRefs(obj), "namespace//team-p7")
	q, _ := store.QuotaSpecByName(nil, "team-p7")
	must.Nil(t, q, must.Sprint("the pass went on although the namespace is not the tenant's"))

	// With adopt it is taken over like any other: what it held is saved,
	// and it is given back, not deleted, with the tenant.
	spec["adopt"] = []any{"namespace"}
	_, err = c.apply("", structs.TenantKind, "team-p7", spec)
	must.NoError(t, err)
	obj = waitTenant(c, "team-p7", "ready", tenantReady)
	ref := tenantRefs(obj)["namespace//team-p7"]
	must.True(t, ref.Adopted)
	must.False(t, ref.Pending)
	must.Eq(t, &tenantOriginal{Description: "made by hand", Meta: map[string]string{"team": "hand"}}, ref.Original)
	ns, _ = store.NamespaceByName(nil, "team-p7")
	must.Eq(t, "Team P7", ns.Description)

	must.NoError(t, c.delete("", structs.TenantKind, "team-p7"))
	waitTenantGone(c, "team-p7")
	ns, _ = store.NamespaceByName(nil, "team-p7")
	must.NotNil(t, ns)
	must.Eq(t, "made by hand", ns.Description)
	q, _ = store.QuotaSpecByName(nil, "team-p7")
	must.Nil(t, q)
}

// The tenant knows its own creation under a pending entry: what it created
// holds what the status says it was creating, for every type, also when the
// status write that clears pending was lost and the spec changed meanwhile.
func TestTenant_PendingOwnCreation(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()

	spec := map[string]any{
		"namespace":    map[string]any{"description": "Team OC", "meta": map[string]any{"owner": "oc"}},
		"quota":        map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 8000, "allocations": 5}}},
		"roles":        map[string]any{"dev": []any{"submit-job"}},
		"network":      map[string]any{"isolation": "namespace", "allow_from_namespaces": []any{"ingress"}},
		"dns_suffixes": []any{"oc.example.com"},
	}
	_, err := c.apply("", structs.TenantKind, "team-oc", spec)
	must.NoError(t, err)
	obj := waitTenant(c, "team-oc", "ready", tenantReady)
	for key, ref := range tenantRefs(obj) {
		must.False(t, ref.Pending, must.Sprint(key))
		must.Eq(t, "", ref.Creating, must.Sprint(key))
	}

	// What each resource holds after its creation is what the tenant set
	// out to create.
	decoded, err := decodeTenantSpec("team-oc", obj.Spec)
	must.NoError(t, err)
	d, err := decoded.desired("team-oc")
	must.NoError(t, err)
	ns, _ := store.NamespaceByName(nil, "team-oc")
	q, _ := store.QuotaSpecByName(nil, "team-oc")
	pol, _ := store.ACLPolicyByName(nil, "team-oc-dev")
	role, _ := store.GetACLRoleByName(nil, "team-oc-dev")
	np, _ := store.NetworkPolicyByName(nil, "team-oc", structs.TenantNetworkPolicyName)
	created := map[string]string{
		"namespace//team-oc":                      namespaceOriginal(ns).fingerprint(),
		"quota//team-oc":                          quotaOriginal(q).fingerprint(),
		"acl-policy//team-oc-dev":                 policyOriginal(pol).fingerprint(),
		"acl-role//team-oc-dev":                   roleOriginal(role).fingerprint(),
		"network-policy/team-oc/tenant-isolation": networkPolicyOriginal(np).fingerprint(),
	}
	must.Eq(t, map[string]string{
		"namespace//team-oc":                      namespaceOriginal(d.namespace).fingerprint(),
		"quota//team-oc":                          quotaOriginal(d.quota).fingerprint(),
		"acl-policy//team-oc-dev":                 policyOriginal(d.policies[0]).fingerprint(),
		"acl-role//team-oc-dev":                   roleOriginal(d.roles[0]).fingerprint(),
		"network-policy/team-oc/tenant-isolation": networkPolicyOriginal(d.network).fingerprint(),
	}, created)

	// The status as a lost write leaves it: everything created, everything
	// still pending; and the spec changed meanwhile.
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		changed := c.get("", structs.TenantKind, "team-oc").Copy()
		refs := tenantRefs(changed)
		for key, ref := range refs {
			ref.Pending, ref.Creating = true, created[key]
			refs[key] = ref
		}
		changed.Status["resources"] = statusRefs(refs)
		changed.Spec = structs.CopyObjectMap(changed.Spec)
		changed.Spec["namespace"] = map[string]any{"description": "Team OC, renamed", "meta": map[string]any{"owner": "oc"}}
		changed.Generation++
		_, err := c.s.writeOperatorObject(changed)
		return err
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	obj = waitTenant(c, "team-oc", "ready with the new spec", func(o *structs.CustomObject) bool {
		ns, _ := store.NamespaceByName(nil, "team-oc")
		return tenantReady(o) && ns != nil && ns.Description == "Team OC, renamed"
	})
	must.False(t, objectConditionTrue(obj.Status, structs.TenantConditionConflict))
	refs := tenantRefs(obj)
	must.MapLen(t, 5, refs)
	for key, ref := range refs {
		must.False(t, ref.Pending, must.Sprint(key))
		must.False(t, ref.Adopted, must.Sprint(key))
		must.Eq(t, "", ref.Creating, must.Sprint(key))
	}
}

// Between a revoked leadership and the one established after it the leader
// has no ACL token: a pass of the tenant operator then writes nothing, and
// records no failure of a tenant.
func TestTenant_WaitsForTheLeaderToken(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()

	token := c.s.getLeaderAcl()
	must.NotEq(t, "", token)
	c.s.setLeaderAcl("")
	_, err := c.apply("", structs.TenantKind, "team-gap", map[string]any{
		"namespace": map[string]any{"description": "written in the gap"},
	})
	must.NoError(t, err)
	time.Sleep(2 * time.Second)
	obj := c.get("", structs.TenantKind, "team-gap")
	must.NotNil(t, obj)
	must.False(t, objectConditionTrue(obj.Status, structs.CertificateConditionDegraded),
		must.Sprintf("a failure was recorded without the leader token: %v", obj.Status))
	must.Eq(t, 0, len(obj.Status), must.Sprintf("status: %v", obj.Status))

	c.s.setLeaderAcl(token)
	waitTenant(c, "team-gap", "ready with the token", tenantReady)
}

// Whether a resource of a tenant is gone is read from the state, never from
// the text of an error: a refused deletion whose message names something
// like "nonexistent" is not a deletion (review finding SEC-13).
func TestTenant_DeleteResourceRefused(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	o := newTenantOperator(c.s, c.s.logger)
	nextIndex := func() uint64 {
		index, err := store.LatestIndex()
		must.NoError(t, err)
		return index + 1
	}

	tenantByHand{c}.namespace("held", "", nil)
	// The name of what holds the namespace is in the refusal, and is the
	// choice of whoever may write into the namespace.
	volume := mock.CSIVolume(mock.CSIPlugin())
	volume.ID, volume.Namespace = "nonexistent-not-found", "held"
	must.NoError(t, store.UpsertCSIVolume(nextIndex(), []*structs.CSIVolume{volume}))

	ref := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "held"}
	err := o.deleteResource(ref)
	must.Error(t, err, must.Sprint("a refused deletion of the namespace counted as done"))
	must.StrContains(t, err.Error(), "contains at least one CSI volume")
	ns, _ := store.NamespaceByName(nil, "held")
	must.NotNil(t, ns)

	// A namespace that is gone already is fine.
	must.NoError(t, o.deleteResource(tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "never-there"}))
	must.NoError(t, o.deleteResource(tenantResourceRef{Type: structs.TenantResourceQuota, Name: "never-there"}))
	must.NoError(t, o.deleteResource(tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: "never-there"}))
	must.NoError(t, o.deleteResource(tenantResourceRef{Type: structs.TenantResourceACLRole, Name: "never-there"}))
	must.NoError(t, o.deleteResource(tenantResourceRef{
		Type: structs.TenantResourceNetworkPolicy, Namespace: "held", Name: structs.TenantNetworkPolicyName}))
}

// A deletion of the namespace that is refused deletes nothing else of the
// tenant: the network policy, the roles and the policies stay (review
// finding SEC-13).
func TestTenant_RefusedNamespaceDeletesNothing(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	o := newTenantOperator(c.s, c.s.logger)
	hand := tenantByHand{c}

	hand.namespace("planted", "", nil)
	hand.networkPolicy("planted")
	hand.policy("planted-dev", `node { policy = "read" }`)
	var roles structs.ACLRolesUpsertResponse
	must.NoError(t, c.s.RPC(structs.ACLUpsertRolesRPCMethod, &structs.ACLRolesUpsertRequest{
		ACLRoles: []*structs.ACLRole{{Name: "planted-dev",
			Policies: []*structs.ACLRolePolicyLink{{Name: "planted-dev"}}}},
		WriteRequest: c.write(""),
	}, &roles))

	// Written into the namespace after the tenant looked into it: the
	// status below has no namespace, so the deletion does not look first,
	// as a write between the look and the deletion would have it.
	variable := mock.VariableEncrypted()
	variable.Namespace = "planted"
	index, err := store.LatestIndex()
	must.NoError(t, err)
	must.NoError(t, store.VarSet(index+1, &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: variable}).Error)

	// A deleted tenant that created all of them; the store does not have
	// the object, so the loop of the operators never sees it.
	ghost := &structs.CustomObject{
		ID: uuid.Generate(), Group: structs.OperatorsGroup, Kind: structs.TenantKind, Name: "planted",
		Generation: 1, ModifyIndex: 7, DeletionTime: time.Now().UnixNano(),
		Finalizers: []string{structs.OperatorsFinalizer},
		Status: map[string]any{"resources": statusRefs(map[string]tenantResourceRef{
			"ns":   {Type: structs.TenantResourceNamespace, Name: "planted"},
			"np":   {Type: structs.TenantResourceNetworkPolicy, Namespace: "planted", Name: structs.TenantNetworkPolicyName},
			"pol":  {Type: structs.TenantResourceACLPolicy, Name: "planted-dev"},
			"role": {Type: structs.TenantResourceACLRole, Name: "planted-dev"},
		})},
	}
	o.finishDeletion(newTenantPass(t, c.s), ghost)

	ns, _ := store.NamespaceByName(nil, "planted")
	must.NotNil(t, ns)
	np, _ := store.NetworkPolicyByName(nil, "planted", structs.TenantNetworkPolicyName)
	must.NotNil(t, np, must.Sprint("the isolation went although the namespace could not be deleted"))
	role, _ := store.GetACLRoleByName(nil, "planted-dev")
	must.NotNil(t, role, must.Sprint("the role went although the namespace could not be deleted"))
	pol, _ := store.ACLPolicyByName(nil, "planted-dev")
	must.NotNil(t, pol, must.Sprint("the policy went although the namespace could not be deleted"))

	// With the namespace empty everything goes, the network policy after
	// its namespace.
	must.NoError(t, store.VarDelete(index+2, &structs.VarApplyStateRequest{Op: structs.VarOpDelete, Var: variable}).Error)
	o.finishDeletion(newTenantPass(t, c.s), ghost)
	ns, _ = store.NamespaceByName(nil, "planted")
	must.Nil(t, ns)
	np, _ = store.NetworkPolicyByName(nil, "planted", structs.TenantNetworkPolicyName)
	must.Nil(t, np)
	role, _ = store.GetACLRoleByName(nil, "planted-dev")
	must.Nil(t, role)
	pol, _ = store.ACLPolicyByName(nil, "planted-dev")
	must.Nil(t, pol)
}
