// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// newTenantTest returns a test server with ACLs, driven with its root token.
func newTenantTest(t *testing.T) (*certTest, func()) {
	t.Helper()
	s, root, cleanup := TestACLServer(t, nil)
	testutil.WaitForLeader(t, s.RPC)
	// The leader registers the built-in kinds after it is elected, on its
	// own time: a tenant written before that is refused.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		kind, err := s.State().ObjectKindByName(nil, structs.OperatorsGroup, structs.TenantKind)
		return err == nil && kind != nil
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the kind Tenant is registered"))
	return &certTest{t: t, s: s, token: root.SecretID}, cleanup
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
	), must.Sprintf("waiting for the tenant %s: %s; object: %v", name, what,
		func() any { return c.get("", structs.TenantKind, name) }()))
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

	// A pass with nothing changed writes nothing: no endless rewrite.
	before := c.get("", structs.TenantKind, "team-a").ModifyIndex
	nsBefore := ns.ModifyIndex
	time.Sleep(2 * time.Second)
	must.Eq(t, before, c.get("", structs.TenantKind, "team-a").ModifyIndex)
	ns, _ = store.NamespaceByName(nil, "team-a")
	must.Eq(t, nsBefore, ns.ModifyIndex)

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
		must.Sprintf("tenant still there: %v", func() any { return c.get("", structs.TenantKind, "team-a") }()))

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
