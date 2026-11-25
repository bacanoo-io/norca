// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// TestRelease_UpgradeLeavesOwnedUnchangedResources checks the one exception
// to "the caller may write every resource of the pack": a resource the
// release owns already and that the pack leaves as it is. An operator
// installs a pack with a namespace, a quota and an ACL policy; the team of
// the namespace of the release, which may write none of the three, upgrades
// and rolls back the release as long as the three stay as they are.
func TestRelease_UpgradeLeavesOwnedUnchangedResources(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	f := newForeignResources(t, c)
	store := c.srv.State()
	team := mock.CreatePolicyAndToken(t, store, 1000, "team-a", releaseTenantA)

	const rules = `namespace "a" { policy = "read" }`
	pack := func(count int, policyRules string) []*structs.ReleaseResource {
		job := testReleaseJob("web")
		job.TaskGroups[0].Count = count
		return []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "app-ns", Description: "of the app"}},
			{Type: structs.ReleaseResourceQuota, Quota: &structs.QuotaSpec{
				Name:   "app-quota",
				Limits: []*structs.QuotaLimit{{Region: "global", RegionLimit: &structs.Resources{CPU: 10000, MemoryMB: 10000}}},
			}},
			{Type: structs.ReleaseResourceACLPolicy, ACLPolicy: &structs.ACLPolicy{Name: "app-read", Rules: policyRules}},
			{Type: structs.ReleaseResourceJob, Job: job},
		}
	}
	apply := func(token string, dryRun bool, resources []*structs.ReleaseResource) (*structs.ReleaseApplyResponse, error) {
		return c.applyAs(token, "a", "app", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "app"}, Atomic: pointer.Of(false), DryRun: dryRun, Resources: resources,
		})
	}
	actions := func(resp *structs.ReleaseApplyResponse) map[string]string {
		out := map[string]string{}
		for _, ch := range resp.Changes {
			out[ch.Type] = ch.Action
			if ch.Error != "" {
				out[ch.Type] += ": " + ch.Error
			}
		}
		return out
	}
	global := pack(1, rules)[:3]
	indexes := func() []uint64 {
		var out []uint64
		for _, res := range global {
			out = append(out, f.index(t, res))
		}
		return out
	}
	count := func() int {
		job, err := store.JobByID(nil, "a", "web")
		must.NoError(t, err)
		must.NotNil(t, job)
		return job.TaskGroups[0].Count
	}
	policyRules := func() string {
		p, err := store.ACLPolicyByName(nil, "app-read")
		must.NoError(t, err)
		return p.Rules
	}

	// The team can't install the pack: the three are not its to write.
	_, err := apply(team.SecretID, false, pack(1, rules))
	must.ErrorContains(t, err, "Permission denied")
	must.Nil(t, c.get("a", "app"))

	// The operator installs it.
	_, err = apply(c.token, false, pack(1, rules))
	must.NoError(t, err)
	must.Len(t, 4, c.get("a", "app").Status.Resources)
	before := indexes()

	// The team plans and applies an upgrade of the job: the three are
	// unchanged, not written, and still the release's.
	want := map[string]string{
		structs.ReleaseResourceNamespace: structs.ReleaseChangeUnchanged,
		structs.ReleaseResourceQuota:     structs.ReleaseChangeUnchanged,
		structs.ReleaseResourceACLPolicy: structs.ReleaseChangeUnchanged,
		structs.ReleaseResourceJob:       structs.ReleaseChangeUpdate,
	}
	plan, err := apply(team.SecretID, true, pack(2, rules))
	must.NoError(t, err)
	must.Eq(t, want, actions(plan))
	resp, err := apply(team.SecretID, false, pack(2, rules))
	must.NoError(t, err)
	must.Eq(t, want, actions(resp))
	must.Eq(t, 2, count())
	must.Eq(t, before, indexes())
	rel := c.get("a", "app")
	must.Eq(t, structs.ReleasePhaseDeployed, rel.Status.Phase)
	must.Eq(t, 2, rel.Status.Revision)
	must.Len(t, 4, rel.Status.Resources)

	// The same upgrade with the policy changed in the pack is refused:
	// nothing is applied, nothing recorded. The plan says so too.
	const wider = `namespace "*" { policy = "write" }`
	plan, err = apply(team.SecretID, true, pack(3, wider))
	must.NoError(t, err)
	must.Eq(t, structs.ReleaseChangeUpdate+": "+structs.ErrPermissionDenied.Error(), actions(plan)[structs.ReleaseResourceACLPolicy])
	must.Eq(t, structs.ReleaseChangeUnchanged, actions(plan)[structs.ReleaseResourceNamespace])
	_, err = apply(team.SecretID, false, pack(3, wider))
	must.ErrorContains(t, err, "Permission denied")
	must.Eq(t, 2, count())
	must.Eq(t, rules, policyRules())
	must.Eq(t, before, indexes())
	rel = c.get("a", "app")
	must.Eq(t, structs.ReleasePhaseDeployed, rel.Status.Phase)
	must.Eq(t, 2, rel.Status.Revision)
	must.Eq(t, 2, rel.Spec.Revision)
	must.Len(t, 2, c.history("a", "app"))

	// A resource of the same content that the release does not own is not
	// excepted: the policy "ops" of the cluster is as the request has it.
	_, err = apply(team.SecretID, false, append(pack(3, rules), f.policy()))
	must.ErrorContains(t, err, "Permission denied")
	must.Eq(t, 2, count())
	must.Len(t, 2, c.history("a", "app"))

	// The team rolls back to the revision of the operator.
	rollback := &structs.ReleaseRollbackRequest{Name: "app", Revision: 1, Atomic: pointer.Of(false),
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "a", AuthToken: team.SecretID}}
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Release.Rollback", rollback, new(structs.ReleaseApplyResponse)))
	must.Eq(t, 1, count())
	must.Eq(t, before, indexes())
	must.Eq(t, 3, c.get("a", "app").Status.Revision)

	// Once the live policy is not the one of the pack any more, the pack
	// would write it: that needs the right again.
	var generic structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
		Policies:     []*structs.ACLPolicy{{Name: "app-read", Rules: `namespace "a" { policy = "deny" }`}},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &generic))
	_, err = apply(team.SecretID, false, pack(2, rules))
	must.ErrorContains(t, err, "Permission denied")
	must.Eq(t, 1, count())
	must.Eq(t, 3, c.get("a", "app").Status.Revision)
	must.Len(t, 3, c.history("a", "app"))
	// The operator's upgrade writes it.
	_, err = apply(c.token, false, pack(2, rules))
	must.NoError(t, err)
	must.Eq(t, rules, policyRules())
}

// TestRelease_VariableNeedsWriteOnly checks that a token that may write a
// Variables entry but not read it deploys a pack with the entry: the entry
// is written through its endpoint and nothing of its stored items comes
// back, in the plan or in the release.
func TestRelease_VariableNeedsWriteOnly(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	newForeignResources(t, c)
	store := c.srv.State()
	writeOnly := mock.CreatePolicyAndToken(t, store, 1000, "team-a-write-only", `
namespace "a" {
  policy = "write"
  object "rigger.norca.io/Release" { capabilities = ["read", "write"] }
  variables {
    path "*" { capabilities = ["write"] }
  }
}`)

	const stored = "stored-canary-77b2"
	pack := func(value string) []*structs.ReleaseResource {
		return []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceVariable, Variable: &structs.VariableDecrypted{
				VariableMetadata: structs.VariableMetadata{Path: "config/app"},
				Items:            map[string]string{"k": value},
			}},
			{Type: structs.ReleaseResourceJob, Job: testReleaseJob("vars-app")},
		}
	}
	apply := func(dryRun bool, value string) (*structs.ReleaseApplyResponse, error) {
		return c.applyAs(writeOnly.SecretID, "a", "vars", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "vars"}, Atomic: pointer.Of(false), DryRun: dryRun, Resources: pack(value),
		})
	}
	live := func() string {
		snap, err := store.Snapshot()
		must.NoError(t, err)
		res, err := c.srv.releases.readLive(snap, &structs.ReleaseResourceRef{
			Type: structs.ReleaseResourceVariable, Namespace: "a", Name: "config/app"})
		must.NoError(t, err)
		must.True(t, res.exists())
		return res.res.Variable.Items["k"]
	}

	// An entry of that path exists, written by someone else.
	var varReply structs.VariablesApplyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Variables.Apply", &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: "a", Path: "config/app"},
			Items:            map[string]string{"k": stored},
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "a", AuthToken: c.token},
	}, &varReply))

	// The plan is the same whether the guess is right or wrong, and says
	// nothing of the stored entry.
	var plans []string
	for _, guess := range []string{stored, "another"} {
		plan, err := apply(true, guess)
		must.NoError(t, err)
		must.Eq(t, structs.ReleaseResourceVariable, plan.Changes[0].Type)
		must.Eq(t, structs.ReleaseChangeUpdate, plan.Changes[0].Action)
		must.Eq(t, "", plan.Changes[0].Error)
		plans = append(plans, plan.Changes[0].Action+plan.Changes[0].Error)
	}
	must.Eq(t, plans[0], plans[1])
	must.Eq(t, stored, live())

	// The apply writes the entry; the release records it with a keyed hash.
	resp, err := apply(false, "v1")
	must.NoError(t, err)
	must.Eq(t, structs.ReleaseChangeUpdate, resp.Changes[0].Action)
	must.Eq(t, "v1", live())
	rel := c.get("a", "vars")
	must.Eq(t, structs.ReleasePhaseDeployed, rel.Status.Phase)
	must.Len(t, 2, rel.Status.Resources)
	must.Eq(t, structs.ReleaseResourceVariable, rel.Status.Resources[0].Type)
	must.True(t, strings.HasPrefix(rel.Status.Resources[0].Hash, releaseVariableHashPrefix), must.Sprint(rel.Status.Resources[0].Hash))

	// And upgrades it.
	_, err = apply(false, "v2")
	must.NoError(t, err)
	must.Eq(t, "v2", live())

	// A Variables entry of another namespace is still not this token's.
	foreign := pack("x")
	foreign[0].Variable.Namespace = "b"
	_, err = c.applyAs(writeOnly.SecretID, "a", "vars-b", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "vars"}, Atomic: pointer.Of(false), Resources: foreign,
	})
	must.ErrorContains(t, err, "Permission denied")
	must.Nil(t, c.get("a", "vars-b"))
}
