// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/rpc"
	"strings"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

type releaseTestClient struct {
	t     *testing.T
	srv   *Server
	codec rpc.ClientCodec
	token string
}

func newReleaseTestClient(t *testing.T) (*releaseTestClient, func()) {
	s, root, cleanup := TestACLServer(t, nil)
	testutil.WaitForLeader(t, s.RPC)
	c := &releaseTestClient{t: t, srv: s, codec: rpcClient(t, s), token: root.SecretID}
	// The built-in kinds are registered by the leader.
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		return s.releases.ensureKinds()
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	return c, cleanup
}

func (c *releaseTestClient) apply(ns, name string, req *structs.ReleaseApplyRequest) (*structs.ReleaseApplyResponse, error) {
	req.Name = name
	req.WriteRequest = structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: c.token}
	var resp structs.ReleaseApplyResponse
	err := msgpackrpc.CallWithCodec(c.codec, "Release.Apply", req, &resp)
	return &resp, err
}

func (c *releaseTestClient) get(ns, name string) *structs.Release {
	req := &structs.ReleaseSpecificRequest{Name: name, QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns, AuthToken: c.token}}
	var resp structs.SingleReleaseResponse
	must.NoError(c.t, msgpackrpc.CallWithCodec(c.codec, "Release.GetRelease", req, &resp))
	return resp.Release
}

func (c *releaseTestClient) history(ns, name string) []*structs.ReleaseRevision {
	req := &structs.ReleaseSpecificRequest{Name: name, QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns, AuthToken: c.token}}
	var resp structs.ReleaseHistoryResponse
	must.NoError(c.t, msgpackrpc.CallWithCodec(c.codec, "Release.History", req, &resp))
	return resp.Revisions
}

func (c *releaseTestClient) revision(ns, name string, n uint64) *structs.ReleaseRevision {
	req := &structs.ReleaseRevisionRequest{Name: name, Revision: n, QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns, AuthToken: c.token}}
	var resp structs.SingleReleaseRevisionResponse
	must.NoError(c.t, msgpackrpc.CallWithCodec(c.codec, "Release.GetRevision", req, &resp))
	return resp.Revision
}

func (c *releaseTestClient) rollback(ns, name string, rev uint64) (*structs.ReleaseApplyResponse, error) {
	req := &structs.ReleaseRollbackRequest{Name: name, Revision: rev,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: c.token}}
	var resp structs.ReleaseApplyResponse
	err := msgpackrpc.CallWithCodec(c.codec, "Release.Rollback", req, &resp)
	return &resp, err
}

func (c *releaseTestClient) drift(ns, name string) *structs.ReleaseDriftResponse {
	req := &structs.ReleaseDriftRequest{Name: name, WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: c.token}}
	var resp structs.ReleaseDriftResponse
	must.NoError(c.t, msgpackrpc.CallWithCodec(c.codec, "Release.Drift", req, &resp))
	return &resp
}

func (c *releaseTestClient) uninstall(ns, name, propagation string) (*structs.ReleaseDeleteResponse, error) {
	req := &structs.ReleaseDeleteRequest{Name: name, Propagation: propagation,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: c.token}}
	var resp structs.ReleaseDeleteResponse
	err := msgpackrpc.CallWithCodec(c.codec, "Release.Delete", req, &resp)
	return &resp, err
}

func testReleaseJob(id string) *structs.Job {
	job := mock.Job()
	job.ID, job.Name = id, id
	job.Namespace = ""
	job.TaskGroups[0].Count = 1
	return job
}

const testReleaseKindSchema = `{"type":"object","properties":{"spec":{"type":"object","properties":{"size":{"type":"integer","default":1}}}}}`

// testReleasePack returns the resources of a pack with a namespace, a quota
// referenced by the namespace, an ACL policy, a Variables entry with a
// secret item, an object kind with an object, and two jobs.
func testReleasePack(prefix string) []*structs.ReleaseResource {
	return []*structs.ReleaseResource{
		{Type: structs.ReleaseResourceJob, Job: testReleaseJob(prefix + "-web")},
		{Type: structs.ReleaseResourceJob, Job: testReleaseJob(prefix + "-api")},
		{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{
			Name: prefix + "-ns", Description: "team namespace", Quota: prefix + "-quota",
		}},
		{Type: structs.ReleaseResourceQuota, Quota: &structs.QuotaSpec{
			Name:   prefix + "-quota",
			Limits: []*structs.QuotaLimit{{Region: "global", RegionLimit: &structs.Resources{CPU: 10000, MemoryMB: 10000}}},
		}},
		{Type: structs.ReleaseResourceACLPolicy, ACLPolicy: &structs.ACLPolicy{
			Name: prefix + "-read", Rules: `namespace "default" { policy = "read" }`,
		}},
		{Type: structs.ReleaseResourceVariable, SecretKeys: []string{"password"}, Variable: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Path: "config/" + prefix},
			Items:            map[string]string{"user": "admin"},
		}},
		{Type: structs.ReleaseResourceObjectKind, ObjectKind: &structs.ObjectKind{
			Group: "test.example.com", Kind: "Widget", Namespaced: true, Schema: testReleaseKindSchema,
		}},
		{Type: structs.ReleaseResourceObject, Object: &structs.CustomObject{
			Group: "test.example.com", Kind: "Widget", Name: prefix + "-widget", Spec: map[string]any{"size": 2},
		}},
	}
}

func testReleaseSecrets(prefix string) []*structs.ReleaseVariableSecret {
	return []*structs.ReleaseVariableSecret{{Path: "config/" + prefix, Items: map[string]string{"password": "s3cret"}}}
}

func TestRelease_InstallUpgradeUninstall(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	store := c.srv.State()

	// Install everything in one step.
	resp, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "app", Version: "1.0.0"},
		Resources: testReleasePack("app"),
		Secrets:   testReleaseSecrets("app"),
		Values:    map[string]any{"count": 1},
	})
	must.NoError(t, err)
	must.Eq(t, 1, resp.Revision)
	rel := resp.Release
	must.Eq(t, structs.ReleasePhaseDeployed, rel.Status.Phase)
	must.Eq(t, 1, rel.Status.Revision)
	must.Len(t, 8, rel.Status.Resources)

	ns, _ := store.NamespaceByName(nil, "app-ns")
	must.NotNil(t, ns)
	must.Eq(t, "app-quota", ns.Quota)
	q, _ := store.QuotaSpecByName(nil, "app-quota")
	must.NotNil(t, q)
	p, _ := store.ACLPolicyByName(nil, "app-read")
	must.NotNil(t, p)
	job, _ := store.JobByID(nil, "default", "app-web")
	must.NotNil(t, job)
	must.Eq(t, "app", job.Meta[structs.ReleaseMetaRelease])
	obj, _ := store.CustomObjectByName(nil, "test.example.com", "Widget", "default", "app-widget")
	must.NotNil(t, obj)
	must.Len(t, 1, obj.Owners)
	must.Eq(t, structs.ReleaseKindRef, obj.Owners[0].Kind)
	must.Eq(t, rel.ID, obj.Owners[0].ID)

	snap, _ := store.Snapshot()
	live, err := c.srv.releases.readLive(snap, &structs.ReleaseResourceRef{Type: structs.ReleaseResourceVariable, Namespace: "default", Name: "config/app"})
	must.NoError(t, err)
	must.Eq(t, map[string]string{"user": "admin", "password": "s3cret"}, live.res.Variable.Items)

	// The secret is not part of the revision.
	rev1 := c.revision("default", "app", 1)
	must.Eq(t, structs.ReleaseActionInstall, rev1.Spec.Action)
	must.Len(t, 8, rev1.Resources)
	for _, res := range rev1.Resources {
		if res.Type == structs.ReleaseResourceVariable {
			must.Eq(t, map[string]string{"user": "admin"}, res.Variable.Items)
			must.Eq(t, []string{"password"}, res.SecretKeys)
		}
	}
	must.StrNotContains(t, rev1.Spec.Content, "s3cret")

	// Plan an upgrade: one job changes, the Variables entry is removed.
	upgrade := testReleasePack("app")
	upgrade[0].Job.TaskGroups[0].Count = 2
	upgrade = append(upgrade[:5], upgrade[6:]...)
	plan, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app", Version: "1.1.0"}, Resources: upgrade, DryRun: true,
	})
	must.NoError(t, err)
	actions := map[string]string{}
	for _, ch := range plan.Changes {
		actions[ch.Type+":"+ch.Name] = ch.Action
	}
	must.Eq(t, structs.ReleaseChangeUpdate, actions["job:app-web"])
	must.Eq(t, structs.ReleaseChangeUnchanged, actions["job:app-api"])
	must.Eq(t, structs.ReleaseChangeUnchanged, actions["namespace:app-ns"])
	must.Eq(t, structs.ReleaseChangeUnchanged, actions["object:test.example.com/Widget/app-widget"])
	must.Eq(t, structs.ReleaseChangeDelete, actions["variable:config/app"])

	resp, err = c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app", Version: "1.1.0"}, Resources: upgrade,
	})
	must.NoError(t, err)
	must.Eq(t, 2, resp.Revision)
	job, _ = store.JobByID(nil, "default", "app-web")
	must.Eq(t, 2, job.TaskGroups[0].Count)
	v, _ := store.GetVariable(nil, "default", "config/app")
	must.Nil(t, v)

	hist := c.history("default", "app")
	must.Len(t, 2, hist)
	must.Eq(t, 2, hist[0].Revision)
	must.Eq(t, structs.ReleasePhaseDeployed, hist[0].Status.Phase)
	must.Eq(t, structs.ReleasePhaseSuperseded, hist[1].Status.Phase)
	must.Eq(t, "", hist[0].Spec.Content)

	// Uninstall removes everything, in reverse order.
	del, err := c.uninstall("default", "app", "")
	must.NoError(t, err)
	var order []string
	for _, ch := range del.Changes {
		must.Eq(t, structs.ReleaseChangeDelete, ch.Action)
		must.Eq(t, "", ch.Error)
		order = append(order, ch.Type)
	}
	must.Eq(t, []string{"job", "job", "object", "object-kind", "acl-policy", "quota", "namespace"}, order)
	ns, _ = store.NamespaceByName(nil, "app-ns")
	must.Nil(t, ns)
	job, _ = store.JobByID(nil, "default", "app-web")
	must.Nil(t, job)
	must.Nil(t, c.get("default", "app"))
	revObj, _ := store.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.ReleaseRevisionKind, "default", "app.1")
	must.Nil(t, revObj)
}

func TestRelease_RollbackHistoryAndPruning(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	store := c.srv.State()

	for i := 1; i <= 4; i++ {
		job := testReleaseJob("roll")
		job.Meta = map[string]string{"build": fmt.Sprint(i)}
		resp, err := c.apply("default", "roll", &structs.ReleaseApplyRequest{
			Pack:       &structs.ReleasePack{Name: "roll", Version: fmt.Sprintf("1.%d.0", i)},
			Resources:  []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: job}},
			MaxHistory: 3,
		})
		must.NoError(t, err)
		must.Eq(t, uint64(i), resp.Revision)
	}
	hist := c.history("default", "roll")
	must.Len(t, 3, hist)
	must.Eq(t, []uint64{4, 3, 2}, []uint64{hist[0].Revision, hist[1].Revision, hist[2].Revision})

	// Roll back to revision 2: a new revision with its content.
	resp, err := c.rollback("default", "roll", 2)
	must.NoError(t, err)
	must.Eq(t, 5, resp.Revision)
	job, _ := store.JobByID(nil, "default", "roll")
	must.Eq(t, "2", job.Meta["build"])
	rev := c.revision("default", "roll", 5)
	must.Eq(t, structs.ReleaseActionRollback, rev.Spec.Action)
	must.Eq(t, 2, rev.Spec.RollbackOf)
	must.Eq(t, "1.2.0", rev.Spec.Pack.Version)

	// Without a revision, the previous one is restored.
	resp, err = c.rollback("default", "roll", 0)
	must.NoError(t, err)
	must.Eq(t, 6, resp.Revision)
	job, _ = store.JobByID(nil, "default", "roll")
	must.Eq(t, "4", job.Meta["build"])

	// Pruned revisions can't be restored.
	_, err = c.rollback("default", "roll", 1)
	must.ErrorContains(t, err, "not found")
}

func TestRelease_OwnershipConflict(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	res := func() []*structs.ReleaseResource {
		return []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("shared")}}
	}
	_, err := c.apply("default", "first", &structs.ReleaseApplyRequest{Pack: &structs.ReleasePack{Name: "a"}, Resources: res()})
	must.NoError(t, err)

	_, err = c.apply("default", "second", &structs.ReleaseApplyRequest{Pack: &structs.ReleasePack{Name: "b"}, Resources: res()})
	must.ErrorContains(t, err, "belong to other releases")

	_, err = c.apply("default", "second", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "b"}, Resources: res(), TakeOwnership: true,
	})
	must.NoError(t, err)
	must.Len(t, 0, c.get("default", "first").Status.Resources)
	must.Len(t, 1, c.get("default", "second").Status.Resources)
}

func TestRelease_AtomicRestoresOnFailure(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	store := c.srv.State()

	pack := func(desc string, badJob bool) []*structs.ReleaseResource {
		job := testReleaseJob("atomic")
		if badJob {
			job.TaskGroups[0].Tasks[0].Resources.CPU = -5
		}
		return []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "atomic-ns", Description: desc}},
			{Type: structs.ReleaseResourceACLPolicy, ACLPolicy: &structs.ACLPolicy{Name: "atomic-pol", Rules: `namespace "default" { policy = "read" }`}},
			{Type: structs.ReleaseResourceJob, Job: job},
		}
	}
	_, err := c.apply("default", "atomic", &structs.ReleaseApplyRequest{Pack: &structs.ReleasePack{Name: "atomic"}, Resources: pack("v1", false)})
	must.NoError(t, err)

	// The upgrade changes the namespace, removes the policy and fails on the
	// job: everything returns to revision 1.
	upgrade := pack("v2", true)
	upgrade = append(upgrade[:1], upgrade[2:]...)
	_, err = c.apply("default", "atomic", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "atomic"}, Resources: upgrade, Atomic: pointer.Of(true),
	})
	must.ErrorContains(t, err, "rolled back to revision 1")

	ns, _ := store.NamespaceByName(nil, "atomic-ns")
	must.Eq(t, "v1", ns.Description)
	pol, _ := store.ACLPolicyByName(nil, "atomic-pol")
	must.NotNil(t, pol)

	rel := c.get("default", "atomic")
	must.Eq(t, structs.ReleasePhaseFailed, rel.Status.Phase)
	must.Eq(t, 1, rel.Status.Revision)
	must.Eq(t, 2, rel.Spec.Revision)
	must.True(t, rel.Status.LastOperation.RolledBack)
	must.Eq(t, "RolledBack", rel.Status.Condition(structs.ReleaseConditionReady).Reason)
	hist := c.history("default", "atomic")
	must.Eq(t, structs.ReleasePhaseFailed, hist[0].Status.Phase)
	must.Eq(t, structs.ReleasePhaseDeployed, hist[1].Status.Phase)

	// Without atomic mode the failed revision stays applied as far as it got.
	_, err = c.apply("default", "atomic", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "atomic"}, Resources: upgrade, Atomic: pointer.Of(false),
	})
	must.ErrorContains(t, err, "failed")
	ns, _ = store.NamespaceByName(nil, "atomic-ns")
	must.Eq(t, "v2", ns.Description)
	rel = c.get("default", "atomic")
	must.Eq(t, 3, rel.Status.Revision)
}

func TestRelease_AtomicDefaultInProductionNamespaces(t *testing.T) {
	ci.Parallel(t)
	must.False(t, structs.ReleaseAtomicDefault(&structs.Namespace{Name: "dev"}))
	must.True(t, structs.ReleaseAtomicDefault(&structs.Namespace{Name: "prod", Meta: map[string]string{"environment": "Production"}}))
	must.True(t, structs.ReleaseAtomicDefault(&structs.Namespace{Name: "p", Meta: map[string]string{"environment": "prod"}}))
	must.False(t, structs.ReleaseAtomicDefault(&structs.Namespace{Name: "p", Meta: map[string]string{"environment": "prod", "pack.atomic": "false"}}))
	must.True(t, structs.ReleaseAtomicDefault(&structs.Namespace{Name: "s", Meta: map[string]string{"pack.atomic": "true"}}))

	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	var out structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces:   []*structs.Namespace{{Name: "prod", Meta: map[string]string{"environment": "production"}}},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &out))
	resp, err := c.apply("prod", "svc", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "svc"}, Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("svc")}},
	})
	must.NoError(t, err)
	must.True(t, resp.Release.Spec.Atomic)
	job, _ := c.srv.State().JobByID(nil, "prod", "svc")
	must.NotNil(t, job)
}

func TestRelease_Drift(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	_, err := c.apply("default", "drifty", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "drifty"},
		Resources: []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceJob, Job: testReleaseJob("drifty")},
			{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "drifty-ns"}},
		},
	})
	must.NoError(t, err)
	must.False(t, c.drift("default", "drifty").Drifted)

	// Modify the job by hand.
	job, _ := c.srv.State().JobByID(nil, "default", "drifty")
	changed := job.Copy()
	changed.TaskGroups[0].Count = 3
	var reg structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Job.Register", &structs.JobRegisterRequest{
		Job: changed, WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &reg))
	// Re-registering a resource unchanged is no drift.
	var out structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces:   []*structs.Namespace{{Name: "drifty-ns"}},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &out))

	d := c.drift("default", "drifty")
	must.True(t, d.Drifted)
	must.Len(t, 1, d.Resources)
	must.Eq(t, "drifty", d.Resources[0].Name)
	must.Eq(t, "modified", d.Resources[0].Reason)
	cond := c.get("default", "drifty").Status.Condition(structs.ReleaseConditionDrifted)
	must.Eq(t, structs.ReleaseConditionTrue, cond.Status)
}

func TestRelease_SignatureAdmission(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	must.NoError(t, err)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	var out structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces:   []*structs.Namespace{{Name: "prod"}},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &out))
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "AdmissionPolicy.UpsertAdmissionPolicies", &structs.AdmissionPolicyUpsertRequest{
		AdmissionPolicies: []*structs.AdmissionPolicy{{
			Name:        "signed-packs",
			Match:       &structs.AdmissionPolicyMatch{Namespaces: []string{"prod"}, Resources: []string{structs.AdmissionResourceRelease}},
			TrustedKeys: []string{pubPEM},
			Rules: []*structs.AdmissionPolicyRule{{
				Expression: "release.pack.name != 'forbidden'", Message: "this pack is forbidden",
			}},
		}},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &out))

	digest := "sha256:" + strings.Repeat("ab", 32)
	payload := fmt.Sprintf(`{"critical":{"identity":{"docker-reference":"r/p"},"image":{"docker-manifest-digest":%q},"type":"cosign container image signature"},"optional":null}`, digest)
	sum := sha256.Sum256([]byte(payload))
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	must.NoError(t, err)
	signature := &structs.ReleasePackSignature{Payload: payload, Signature: base64.StdEncoding.EncodeToString(sig)}

	job := func() []*structs.ReleaseResource {
		return []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("signed")}}
	}

	// Unsigned packs are rejected in the protected namespace ...
	_, err = c.apply("prod", "app", &structs.ReleaseApplyRequest{Pack: &structs.ReleasePack{Name: "app", Digest: digest}, Resources: job()})
	must.ErrorContains(t, err, "is not signed")
	_, err = c.apply("prod", "app", &structs.ReleaseApplyRequest{Pack: &structs.ReleasePack{Name: "app"}, Resources: job()})
	must.ErrorContains(t, err, ReleaseAdmissionDeniedPrefix)

	// ... but allowed elsewhere.
	_, err = c.apply("default", "app", &structs.ReleaseApplyRequest{Pack: &structs.ReleasePack{Name: "app"}, Resources: job()})
	must.NoError(t, err)

	// A signature for another digest does not count.
	other := "sha256:" + strings.Repeat("cd", 32)
	_, err = c.apply("prod", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app", Digest: other, Signatures: []*structs.ReleasePackSignature{signature}}, Resources: job(),
	})
	must.ErrorContains(t, err, "not signed by a trusted key")

	// The CEL rules see the release.
	_, err = c.apply("prod", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "forbidden", Digest: digest, Signatures: []*structs.ReleasePackSignature{signature}}, Resources: job(),
	})
	must.ErrorContains(t, err, "this pack is forbidden")

	resp, err := c.apply("prod", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app", Digest: digest, Signatures: []*structs.ReleasePackSignature{signature}}, Resources: job(),
	})
	must.NoError(t, err)
	must.StrHasPrefix(t, "sha256:", resp.Release.Spec.Pack.TrustedSigner)
}

func TestRelease_BuiltinGroupIsProtected(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	var resp structs.CustomObjectWriteResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.Apply", &structs.CustomObjectApplyRequest{
		Object:       &structs.CustomObject{Group: structs.ReleaseObjectGroup, Kind: structs.ReleaseKind, Name: "fake", Spec: map[string]any{}},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &resp)
	must.ErrorContains(t, err, "built in")

	var kresp structs.GenericResponse
	err = msgpackrpc.CallWithCodec(c.codec, "ObjectKind.DeleteObjectKind", &structs.ObjectKindDeleteRequest{
		Group: structs.ReleaseObjectGroup, Kind: structs.ReleaseKind,
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &kresp)
	must.ErrorContains(t, err, "built in")

	// The generic object API can read releases.
	_, err = c.apply("default", "readable", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "r"}, Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("readable")}},
	})
	must.NoError(t, err)
	var get structs.SingleCustomObjectResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "CustomObject.GetCustomObject", &structs.CustomObjectSpecificRequest{
		Group: structs.ReleaseObjectGroup, Kind: structs.ReleaseKind, Name: "readable",
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &get))
	must.NotNil(t, get.Object)
	must.Eq[any](t, "deployed", get.Object.Status["phase"])

	// Operations are recorded in the event log of the release object.
	var events structs.CustomObjectEventsResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "CustomObject.ListEvents", &structs.CustomObjectSpecificRequest{
		Group: structs.ReleaseObjectGroup, Kind: structs.ReleaseKind, Name: "readable",
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &events))
	must.SliceNotEmpty(t, events.Events)
	must.Eq(t, "Deployed", events.Events[len(events.Events)-1].Reason)

	// Jobs carry the owner reference of the operator framework.
	job, _ := c.srv.State().JobByID(nil, "default", "readable")
	must.Eq(t, structs.ReleaseKindRef, job.Meta[structs.JobMetaOwnerKind])
	must.Eq(t, structs.ObjectOwnerMetaValue(structs.ReleaseObjectGroup, structs.ReleaseKind, "default", "readable"), job.Meta[structs.JobMetaOwner])
}

func TestRelease_ACL(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	// A token that can write releases but not submit jobs can't install a
	// pack with a job: applying grants no permissions.
	token := mock.CreatePolicyAndToken(t, c.srv.State(), 1000, "release-only", `
namespace "default" {
  object "packs.norca.io/Release" { capabilities = ["read", "write"] }
}`)
	req := &structs.ReleaseApplyRequest{
		Name: "app", Pack: &structs.ReleasePack{Name: "app"},
		Resources:    []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("acl")}},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token.SecretID},
	}
	var resp structs.ReleaseApplyResponse
	err := msgpackrpc.CallWithCodec(c.codec, "Release.Apply", req, &resp)
	must.ErrorContains(t, err, "Permission denied")
	rel := c.get("default", "app")
	must.Eq(t, structs.ReleasePhaseFailed, rel.Status.Phase)

	// A token without release permissions can't even start.
	token2 := mock.CreatePolicyAndToken(t, c.srv.State(), 1001, "jobs-only", `namespace "default" { policy = "write" }`)
	req.Name = "app2"
	req.AuthToken = token2.SecretID
	err = msgpackrpc.CallWithCodec(c.codec, "Release.Apply", req, &resp)
	must.ErrorContains(t, err, "Permission denied")
	must.Nil(t, c.get("default", "app2"))

	// PK-3: A dry-run plan must not reveal whether a secret Variable exists
	// or matches when the caller lacks Variable permissions.
	store := c.srv.State()
	var varReply structs.VariablesApplyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Variables.Apply", &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: "secret/db"},
			Items:            map[string]string{"pw": "guess"},
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &varReply))
	planReq := &structs.ReleaseApplyRequest{
		Name:   "probe",
		DryRun: true,
		Pack:   &structs.ReleasePack{Name: "probe"},
		Resources: []*structs.ReleaseResource{{
			Type:     structs.ReleaseResourceVariable,
			Variable: &structs.VariableDecrypted{VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: "secret/db"}, Items: map[string]string{"pw": "guess"}},
		}},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token.SecretID},
	}
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Release.Apply", planReq, &resp))
	must.Len(t, 1, resp.Changes)
	must.Eq(t, structs.ReleaseChangeUpdate, resp.Changes[0].Action)
	must.StrContains(t, resp.Changes[0].Error, "Permission denied")

	// PK-4: Reading ReleaseRevision through CustomObject API requires read-job.
	_, err = c.apply("default", "rev-test", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "rev-test"},
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("rev-job")}},
	})
	must.NoError(t, err)
	revOnlyToken := mock.CreatePolicyAndToken(t, store, 1003, "rev-only", `
namespace "default" {
  object "packs.norca.io/ReleaseRevision" { capabilities = ["read"] }
}`)
	var getObj structs.SingleCustomObjectResponse
	err = msgpackrpc.CallWithCodec(c.codec, "CustomObject.GetCustomObject", &structs.CustomObjectSpecificRequest{
		Group:        structs.ReleaseObjectGroup,
		Kind:         structs.ReleaseRevisionKind,
		Name:         "rev-test.1",
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: revOnlyToken.SecretID},
	}, &getObj)
	must.ErrorContains(t, err, "Permission denied")
}

func TestReleaseResources_Encoding(t *testing.T) {
	ci.Parallel(t)
	resources := testReleasePack("enc")
	content, digest, err := structs.EncodeReleaseResources(resources)
	must.NoError(t, err)
	must.StrHasPrefix(t, "sha256:", digest)
	decoded, err := structs.DecodeReleaseResources(content)
	must.NoError(t, err)
	must.Len(t, len(resources), decoded)
	must.Eq(t, resources[0].Job.ID, decoded[0].Job.ID)
	must.Eq(t, resources[5].Variable.Items, decoded[5].Variable.Items)
}
