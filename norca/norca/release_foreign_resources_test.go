// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// releaseTenantA is the policy of a team that may do everything in the
// namespace "a", releases included, and nothing anywhere else.
const releaseTenantA = `
namespace "a" {
  policy = "write"
  object "packs.norca.io/Release" { capabilities = ["read", "write"] }
  variables {
    path "*" { capabilities = ["read", "write", "destroy", "list"] }
  }
}`

// foreignResources are the resources of the test that the team of the
// namespace "a" may not write: of the namespace "b" and of the cluster.
type foreignResources struct {
	c *releaseTestClient

	policyRules string
	kindSchema  string
}

// newForeignResources creates, with the root token and through the endpoints
// of their types, the namespaces "a" and "b", the job "victim" and the
// Variables entry "secret/db" of "b", the ACL policy "ops", the quota "bq"
// and the object kind b.example.com/Gadget.
func newForeignResources(t *testing.T, c *releaseTestClient) *foreignResources {
	t.Helper()
	f := &foreignResources{
		c:           c,
		policyRules: `namespace "b" { policy = "write" }`,
		kindSchema:  testReleaseKindSchema,
	}
	root := structs.WriteRequest{Region: "global", AuthToken: c.token}
	rpc := func(method string, args, reply any) {
		t.Helper()
		must.NoError(t, msgpackrpc.CallWithCodec(c.codec, method, args, reply), must.Sprint(method))
	}
	var generic structs.GenericResponse
	rpc("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces: []*structs.Namespace{{Name: "a"}, {Name: "b"}}, WriteRequest: root}, &generic)
	rpc("ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
		Policies: []*structs.ACLPolicy{f.policy().ACLPolicy}, WriteRequest: root}, &generic)
	rpc("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
		Quotas: []*structs.QuotaSpec{f.quota().Quota}, WriteRequest: root}, &generic)
	rpc("ObjectKind.UpsertObjectKind", &structs.ObjectKindUpsertRequest{
		ObjectKind: f.kind().ObjectKind, WriteRequest: root}, new(structs.ObjectKindUpsertResponse))
	inB := root
	inB.Namespace = "b"
	rpc("Variables.Apply", &structs.VariablesApplyRequest{
		Op: structs.VarOpSet, Var: f.variable("live").Variable, WriteRequest: inB}, new(structs.VariablesApplyResponse))
	rpc("Job.Register", &structs.JobRegisterRequest{Job: f.job("victim").Job, WriteRequest: inB}, new(structs.JobRegisterResponse))
	return f
}

// The resources as a pack would name them. Those compared with the live
// state by the release itself (namespace, policy, quota, kind) are equal to
// the live ones.
func (f *foreignResources) namespace() *structs.ReleaseResource {
	return &structs.ReleaseResource{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "b"}}
}

func (f *foreignResources) policy() *structs.ReleaseResource {
	return &structs.ReleaseResource{Type: structs.ReleaseResourceACLPolicy,
		ACLPolicy: &structs.ACLPolicy{Name: "ops", Rules: f.policyRules}}
}

func (f *foreignResources) quota() *structs.ReleaseResource {
	return &structs.ReleaseResource{Type: structs.ReleaseResourceQuota, Quota: &structs.QuotaSpec{
		Name:   "bq",
		Limits: []*structs.QuotaLimit{{Region: "global", RegionLimit: &structs.Resources{CPU: 10000, MemoryMB: 10000}}},
	}}
}

func (f *foreignResources) kind() *structs.ReleaseResource {
	return &structs.ReleaseResource{Type: structs.ReleaseResourceObjectKind, ObjectKind: &structs.ObjectKind{
		Group: "b.example.com", Kind: "Gadget", Namespaced: true, Schema: f.kindSchema,
	}}
}

func (f *foreignResources) variable(value string) *structs.ReleaseResource {
	return &structs.ReleaseResource{Type: structs.ReleaseResourceVariable, Variable: &structs.VariableDecrypted{
		VariableMetadata: structs.VariableMetadata{Namespace: "b", Path: "secret/db"},
		Items:            map[string]string{"password": value},
	}}
}

func (f *foreignResources) job(id string) *structs.ReleaseResource {
	job := testReleaseJob(id)
	job.Namespace = "b"
	return &structs.ReleaseResource{Type: structs.ReleaseResourceJob, Job: job}
}

// index returns the modify index of the live resource, 0 if it is gone.
func (f *foreignResources) index(t *testing.T, res *structs.ReleaseResource) uint64 {
	t.Helper()
	snap, err := f.c.srv.State().Snapshot()
	must.NoError(t, err)
	live, err := f.c.srv.releases.readLive(snap, res.Ref())
	must.NoError(t, err)
	if !live.exists() {
		return 0
	}
	return live.modifyIndex
}

// recorded lists what a release says it owns.
func recorded(rel *structs.Release) []string {
	if rel == nil {
		return nil
	}
	var out []string
	for _, ref := range rel.Status.Resources {
		out = append(out, ref.String())
	}
	return out
}

// TestRelease_ForeignResourcesAreNotRecorded checks that an apply naming a
// resource its caller may not write changes and records nothing (security
// review finding SEC-5). A failed apply without atomic mode recorded every
// resource it had started on that existed: the release then listed a job, a
// Variables entry, a policy or a quota of another namespace as its own, with
// its index and hash; other releases were refused it, and an operator who
// deleted the release deleted it.
func TestRelease_ForeignResourcesAreNotRecorded(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	f := newForeignResources(t, c)
	store := c.srv.State()
	team := mock.CreatePolicyAndToken(t, store, 1000, "team-a", releaseTenantA)

	attack := func(release string, dryRun bool, resources ...*structs.ReleaseResource) (*structs.ReleaseApplyResponse, error) {
		return c.applyAs(team.SecretID, "a", release, &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "evil"}, Atomic: pointer.Of(false), DryRun: dryRun, Resources: resources,
		})
	}

	changedPolicy := f.policy()
	changedPolicy.ACLPolicy.Rules = `namespace "*" { policy = "write" }`
	changedQuota := f.quota()
	changedQuota.Quota.Limits[0].RegionLimit.CPU = 1
	foreign := map[string]*structs.ReleaseResource{
		"job":      f.job("victim"),
		"variable": f.variable("guess"),
		"policy":   changedPolicy,
		"quota":    changedQuota,
	}
	for name, res := range foreign {
		release := "evil-" + name
		own := testReleaseJob("own-" + name)
		before := f.index(t, res)
		must.Positive(t, before)

		_, err := attack(release, false, &structs.ReleaseResource{Type: structs.ReleaseResourceJob, Job: own}, res)
		must.ErrorContains(t, err, "Permission denied", must.Sprint(name))

		// Nothing is recorded: no release, no revision.
		rel := c.get("a", release)
		must.Nil(t, rel, must.Sprintf("%s: the release records %v", name, recorded(rel)))
		must.Len(t, 0, c.history("a", release), must.Sprint(name))
		// Nothing is applied, not the caller's own job either.
		job, err := store.JobByID(nil, "a", own.ID)
		must.NoError(t, err)
		must.Nil(t, job, must.Sprint(name))
		must.Eq(t, before, f.index(t, res), must.Sprint(name))
	}

	// The resources are still free for a release that may write them.
	_, err := c.apply("b", "legit", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "legit"}, Resources: []*structs.ReleaseResource{f.job("victim")},
	})
	must.NoError(t, err)
	must.Eq(t, []string{`job "victim" (namespace "b")`}, recorded(c.get("b", "legit")))

	// Who owns a resource is not said to a caller who may not write it:
	// not by an apply, not by a dry run.
	_, err = attack("probe", false, f.job("victim"))
	must.ErrorContains(t, err, "Permission denied")
	must.StrNotContains(t, err.Error(), "legit")
	plan, err := attack("probe", true, f.job("victim"))
	must.NoError(t, err)
	must.Len(t, 1, plan.Changes)
	must.StrContains(t, plan.Changes[0].Error, "Permission denied")
	must.Eq(t, "", plan.Changes[0].Owner)
	must.Nil(t, c.get("a", "probe"))

	// A hook job is a job the release runs later, at an uninstall with the
	// token of whoever uninstalls: one in another namespace is refused like
	// any other resource.
	hook := f.job("cleanup")
	hook.Job.Type = structs.JobTypeBatch
	hook.Job.Meta = map[string]string{structs.ReleaseMetaHook: structs.ReleaseHookPreDelete}
	_, err = attack("hooked", false, &structs.ReleaseResource{Type: structs.ReleaseResourceJob, Job: testReleaseJob("hooked-app")}, hook)
	must.ErrorContains(t, err, "Permission denied")
	must.Nil(t, c.get("a", "hooked"))
	job, err := store.JobByID(nil, "a", "hooked-app")
	must.NoError(t, err)
	must.Nil(t, job)
}

// TestRelease_UnchangedNeedsTheCallersRights checks the resources a release
// compares with the live state itself: one equal to the live one is not
// sent to its endpoint, so the release checks the caller (security review
// finding SEC-7). Without the check a caller learned whether a namespace, a
// quota, an ACL policy or an object kind it may not read has the content it
// guessed, and its release became the owner of it.
func TestRelease_UnchangedNeedsTheCallersRights(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	f := newForeignResources(t, c)
	store := c.srv.State()
	team := mock.CreatePolicyAndToken(t, store, 1000, "team-a", releaseTenantA)

	apply := func(token, release string, resources ...*structs.ReleaseResource) (*structs.ReleaseApplyResponse, error) {
		return c.applyAs(token, "a", release, &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "p"}, Atomic: pointer.Of(false), Resources: resources,
		})
	}

	guessedPolicy, guessedQuota, guessedKind, guessedNamespace := f.policy(), f.quota(), f.kind(), f.namespace()
	guessedPolicy.ACLPolicy.Rules = `namespace "b" { policy = "read" }`
	guessedQuota.Quota.Limits[0].RegionLimit.CPU = 1
	guessedKind.ObjectKind.Description = "another"
	guessedNamespace.Namespace.Description = "another"
	for name, pair := range map[string][2]*structs.ReleaseResource{
		"namespace": {f.namespace(), guessedNamespace},
		"policy":    {f.policy(), guessedPolicy},
		"quota":     {f.quota(), guessedQuota},
		"kind":      {f.kind(), guessedKind},
	} {
		equal, different := pair[0], pair[1]
		before := f.index(t, equal)
		must.Positive(t, before, must.Sprint(name))

		// The live content, guessed right: refused, and nothing recorded.
		release := "guess-" + name
		_, errEqual := apply(team.SecretID, release, equal)
		must.ErrorContains(t, errEqual, "Permission denied", must.Sprintf("%s: the release records %v", name, recorded(c.get("a", release))))
		rel := c.get("a", release)
		must.Nil(t, rel, must.Sprintf("%s: the release records %v", name, recorded(rel)))
		// Guessed wrong: the same answer.
		_, errDifferent := apply(team.SecretID, release, different)
		must.ErrorContains(t, errDifferent, "Permission denied", must.Sprint(name))
		must.Eq(t, errEqual.Error(), errDifferent.Error(), must.Sprint(name))
		must.Nil(t, c.get("a", release))
		must.Eq(t, before, f.index(t, equal), must.Sprint(name))
	}

	// A caller who may write and read such a resource still adopts the one
	// that is as the pack wants it, without a write: the object kind for a
	// token with operator write, the others for a management token.
	kindWriter := mock.CreatePolicyAndToken(t, store, 1002, "team-a-kinds", releaseTenantA+`
operator { policy = "write" }`)
	before := f.index(t, f.kind())
	resp, err := apply(kindWriter.SecretID, "with-kind", f.kind(),
		&structs.ReleaseResource{Type: structs.ReleaseResourceJob, Job: testReleaseJob("kind-app")})
	must.NoError(t, err)
	must.Eq(t, structs.ReleaseChangeUnchanged, resp.Changes[0].Action)
	must.Eq(t, before, f.index(t, f.kind()))
	must.Eq(t, []string{`object-kind "b.example.com/Gadget"`, `job "kind-app" (namespace "a")`}, recorded(c.get("a", "with-kind")))
	// The others are not this token's.
	_, err = apply(kindWriter.SecretID, "with-policy", f.policy())
	must.ErrorContains(t, err, "Permission denied")
	must.Nil(t, c.get("a", "with-policy"))

	resp, err = apply(c.token, "admin", f.namespace(), f.policy())
	must.NoError(t, err)
	must.Len(t, 2, resp.Changes)
	for _, change := range resp.Changes {
		must.Eq(t, structs.ReleaseChangeUnchanged, change.Action, must.Sprint(change.Type))
	}
	must.Eq(t, []string{`namespace "b"`, `acl-policy "ops"`}, recorded(c.get("a", "admin")))
}

// TestRelease_FailedApplyRecordsWhatItApplied checks the record of an apply
// without atomic mode that fails half-way for a caller who may write all of
// its resources: the release owns what was applied, and neither the
// resource that failed nor the ones after it, whether they exist or not.
func TestRelease_FailedApplyRecordsWhatItApplied(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	newForeignResources(t, c)
	store := c.srv.State()
	team := mock.CreatePolicyAndToken(t, store, 1000, "team-a", releaseTenantA)

	// The team's own jobs "h2-bad" and "h3-never" exist already, of no
	// release.
	// They are registered through Raft: a create index ahead of the Raft
	// index would read as a job created after the release recorded it.
	for _, id := range []string{"h2-bad", "h3-never"} {
		job := mock.Job()
		job.ID, job.Name, job.Namespace = id, id, "a"
		must.NoError(t, c.srv.RPC("Job.Register", &structs.JobRegisterRequest{
			Job: job, WriteRequest: structs.WriteRequest{Region: "global", Namespace: "a", AuthToken: c.token},
		}, new(structs.JobRegisterResponse)))
	}

	pack := func(bad bool) []*structs.ReleaseResource {
		var out []*structs.ReleaseResource
		for _, id := range []string{"h1-ok", "h2-bad", "h3-never"} {
			job := testReleaseJob(id)
			if bad && id == "h2-bad" {
				job.TaskGroups[0].Tasks[0].Resources.CPU = -5
			}
			out = append(out, &structs.ReleaseResource{Type: structs.ReleaseResourceJob, Job: job})
		}
		return append(out, &structs.ReleaseResource{Type: structs.ReleaseResourceVariable, Variable: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Path: "config/half"},
			Items:            map[string]string{"k": "v"},
		}})
	}
	apply := func(resources []*structs.ReleaseResource) (*structs.ReleaseApplyResponse, error) {
		return c.applyAs(team.SecretID, "a", "half", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "half"}, Atomic: pointer.Of(false), Resources: resources,
		})
	}

	_, err := apply(pack(true))
	must.ErrorContains(t, err, `release "half" failed`)
	rel := c.get("a", "half")
	must.NotNil(t, rel)
	must.Eq(t, structs.ReleasePhaseFailed, rel.Status.Phase)
	must.Eq(t, 1, rel.Status.Revision)
	must.Eq(t, []string{`variable "config/half" (namespace "a")`, `job "h1-ok" (namespace "a")`}, recorded(rel))
	must.Len(t, 1, c.history("a", "half"))
	// The drift check works on that record.
	must.False(t, c.drift("a", "half").Drifted)

	// The fixed pack is an upgrade of the failed one, and owns all four.
	_, err = apply(pack(false))
	must.NoError(t, err)
	rel = c.get("a", "half")
	must.Eq(t, structs.ReleasePhaseDeployed, rel.Status.Phase)
	must.Len(t, 4, rel.Status.Resources)

	// A failed upgrade keeps the resources of the release, the failed one
	// included, which was applied before; the uninstall deletes them all.
	_, err = apply(pack(true))
	must.ErrorContains(t, err, `release "half" failed`)
	must.Len(t, 4, c.get("a", "half").Status.Resources)
	req := &structs.ReleaseDeleteRequest{Name: "half",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "a", AuthToken: team.SecretID}}
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Release.Delete", req, new(structs.ReleaseDeleteResponse)))
	for _, id := range []string{"h1-ok", "h2-bad", "h3-never"} {
		job, err := store.JobByID(nil, "a", id)
		must.NoError(t, err)
		must.True(t, job == nil || job.Stop, must.Sprintf("job %s still runs", id))
	}
	must.Nil(t, c.get("a", "half"))
}
