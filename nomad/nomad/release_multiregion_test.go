// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"net/rpc"
	"slices"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

const testVariablesSchema = `{
  "type": "object",
  "required": ["image"],
  "properties": {
    "image": {"type": "string", "pattern": "^redis:"},
    "app_count": {"type": "integer", "minimum": 1, "maximum": 3}
  }
}`

func TestRelease_VariablesSchema(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	req := func(values map[string]any, dryRun bool) *structs.ReleaseApplyRequest {
		return &structs.ReleaseApplyRequest{
			Pack:             &structs.ReleasePack{Name: "redis"},
			Resources:        []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("vs-redis")}},
			VariablesSchemas: []*structs.ReleaseVariablesSchema{{Pack: "redis", Schema: testVariablesSchema}},
			VariableValues:   map[string]map[string]any{"redis": values},
			DryRun:           dryRun,
		}
	}

	// Values that violate the schema are rejected, also by plans, with the
	// path of the variable.
	for _, dryRun := range []bool{true, false} {
		_, err := c.apply("default", "vs", req(map[string]any{"image": "nginx", "app_count": 5}, dryRun))
		must.ErrorContains(t, err, `the variables of pack "redis" do not match its variables_schema`)
		must.ErrorContains(t, err, "at /app_count:")
		must.ErrorContains(t, err, "at /image:")
	}
	must.Nil(t, c.get("default", "vs"))

	// Unset variables (null) are absent, so required reports them.
	_, err := c.apply("default", "vs", req(map[string]any{"image": nil}, false))
	must.ErrorContains(t, err, "missing property 'image'")

	// An invalid schema is rejected.
	bad := req(map[string]any{"image": "redis:7"}, false)
	bad.VariablesSchemas[0].Schema = `{"type": 5}`
	_, err = c.apply("default", "vs", bad)
	must.ErrorContains(t, err, `the variables_schema of pack "redis" is invalid`)

	// Valid values: the schema is stored in the revision, the values used
	// for the validation are not.
	resp, err := c.apply("default", "vs", req(map[string]any{"image": "redis:7", "app_count": 2, "password": "s3cret"}, false))
	must.NoError(t, err)
	must.Eq(t, 1, resp.Revision)
	rev := c.revision("default", "vs", 1)
	must.Len(t, 1, rev.Spec.VariablesSchemas)
	must.Eq(t, "redis", rev.Spec.VariablesSchemas[0].Pack)
	must.Eq(t, testVariablesSchema, rev.Spec.VariablesSchemas[0].Schema)
	must.Nil(t, rev.Spec.Values)

	// A rollback keeps the schema of the revision it restores.
	_, err = c.apply("default", "vs", req(map[string]any{"image": "redis:8"}, false))
	must.NoError(t, err)
	resp, err = c.rollback("default", "vs", 1)
	must.NoError(t, err)
	rev = c.revision("default", "vs", resp.Revision)
	must.Len(t, 1, rev.Spec.VariablesSchemas)
	must.Eq(t, testVariablesSchema, rev.Spec.VariablesSchemas[0].Schema)
}

// releaseFederation is a federation of two regions with ACLs, so that the
// servers replicate namespaces and ACL policies from the authoritative
// region r1 to r2.
type releaseFederation struct {
	t     *testing.T
	s1    *Server
	s2    *Server
	token string
	codec rpc.ClientCodec // of s2, the coordinating region
}

func newReleaseFederation(t *testing.T) *releaseFederation {
	s1, root, cleanup1 := TestACLServer(t, func(c *Config) {
		c.Region = "r1"
		c.AuthoritativeRegion = "r1"
	})
	t.Cleanup(cleanup1)
	s2, _, cleanup2 := TestACLServer(t, func(c *Config) {
		c.Region = "r2"
		c.AuthoritativeRegion = "r1"
		c.ReplicationBackoff = 20 * time.Millisecond
		c.ReplicationToken = root.SecretID
	})
	t.Cleanup(cleanup2)
	TestJoin(t, s1, s2)
	testutil.WaitForLeader(t, s1.RPC)
	testutil.WaitForLeader(t, s2.RPC)

	// The global management token of r1 is replicated to r2.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		tok, err := s2.State().ACLTokenBySecretID(nil, root.SecretID)
		return err == nil && tok != nil && len(s1.Regions()) == 2 && len(s2.Regions()) == 2
	}), wait.Timeout(20*time.Second), wait.Gap(50*time.Millisecond)))

	return &releaseFederation{t: t, s1: s1, s2: s2, token: root.SecretID, codec: rpcClient(t, s2)}
}

func (f *releaseFederation) apply(region, name string, req *structs.ReleaseApplyRequest) (*structs.ReleaseApplyResponse, error) {
	req.Name = name
	req.WriteRequest = structs.WriteRequest{Region: region, Namespace: "default", AuthToken: f.token}
	var resp structs.ReleaseApplyResponse
	err := msgpackrpc.CallWithCodec(f.codec, "Release.Apply", req, &resp)
	return &resp, err
}

func (f *releaseFederation) get(region, name string) *structs.Release {
	req := &structs.ReleaseSpecificRequest{Name: name,
		QueryOptions: structs.QueryOptions{Region: region, Namespace: "default", AuthToken: f.token}}
	var resp structs.SingleReleaseResponse
	must.NoError(f.t, msgpackrpc.CallWithCodec(f.codec, "Release.GetRelease", req, &resp))
	return resp.Release
}

func (f *releaseFederation) uninstall(region, name string) {
	req := &structs.ReleaseDeleteRequest{Name: name,
		WriteRequest: structs.WriteRequest{Region: region, Namespace: "default", AuthToken: f.token}}
	var resp structs.ReleaseDeleteResponse
	must.NoError(f.t, msgpackrpc.CallWithCodec(f.codec, "Release.Delete", req, &resp))
}

func ownedKeys(rel *structs.Release) []string {
	var out []string
	for _, r := range rel.Status.Resources {
		out = append(out, r.Type+"/"+r.Namespace+"/"+r.Name)
	}
	slices.Sort(out)
	return out
}

// testMultiregionPack returns a pack with a namespace and an ACL policy
// (global resources), a Variables entry, a job in the namespace of the pack
// and a job with a multiregion block for r1 and r2.
func testMultiregionPack() []*structs.ReleaseResource {
	web := testReleaseJob("mr-web")
	web.Namespace = "mr-ns"
	multi := multiregionTestJob([]string{"r1", "r2"}, 1, "")
	multi.ID, multi.Name, multi.Namespace = "mr-multi", "mr-multi", ""
	return []*structs.ReleaseResource{
		{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "mr-ns", Description: "pack namespace"}},
		{Type: structs.ReleaseResourceACLPolicy, ACLPolicy: &structs.ACLPolicy{
			Name: "mr-read", Rules: `namespace "mr-ns" { policy = "read" }`,
		}},
		{Type: structs.ReleaseResourceVariable, Variable: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Path: "config/mr"},
			Items:            map[string]string{"mode": "multi"},
		}},
		{Type: structs.ReleaseResourceJob, Job: web},
		{Type: structs.ReleaseResourceJob, Job: multi},
	}
}

func TestRelease_Multiregion(t *testing.T) {
	ci.Parallel(t)
	f := newReleaseFederation(t)

	// Unknown regions are rejected before anything is applied.
	_, err := f.apply("r2", "mr", &structs.ReleaseApplyRequest{Resources: testMultiregionPack(), Regions: []string{"r1", "nope"}})
	must.ErrorContains(t, err, `region "nope" is not a known region`)

	// A plan covers both regions without writing.
	resp, err := f.apply("r2", "mr", &structs.ReleaseApplyRequest{
		Resources: testMultiregionPack(), Regions: []string{"r2", "r1"}, DryRun: true,
	})
	must.NoError(t, err)
	must.Len(t, 2, resp.Regions)
	must.Nil(t, f.get("r1", "mr"))
	must.Nil(t, f.get("r2", "mr"))

	// Install in both regions, requested from r2: the authoritative region
	// r1 goes first and applies the global resources.
	resp, err = f.apply("r2", "mr", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "mr-pack"},
		Resources: testMultiregionPack(),
		Regions:   []string{"r2", "r1"},
	})
	must.NoError(t, err)
	must.Len(t, 2, resp.Regions)
	must.Eq(t, "r1", resp.Regions[0].Region)
	must.Eq(t, "r2", resp.Regions[1].Region)
	for _, r := range resp.Regions {
		must.Eq(t, "", r.Error, must.Sprintf("region %s", r.Region))
		must.Eq(t, 1, r.Revision)
	}
	for _, w := range resp.Warnings {
		must.StrNotContains(t, w, "was not replicated")
	}
	must.Eq(t, "r1", resp.Release.Spec.Multiregion.GlobalRegion)

	rel1, rel2 := f.get("r1", "mr"), f.get("r2", "mr")
	must.Eq(t, structs.ReleasePhaseDeployed, rel1.Status.Phase)
	must.Eq(t, structs.ReleasePhaseDeployed, rel2.Status.Phase)
	must.Eq(t, []string{
		"acl-policy//mr-read", "job/default/mr-multi", "job/mr-ns/mr-web", "namespace//mr-ns", "variable/default/config/mr",
	}, ownedKeys(rel1))
	// r2 doesn't own the global resources and adopts the multiregion job.
	must.Eq(t, []string{"job/default/mr-multi", "job/mr-ns/mr-web", "variable/default/config/mr"}, ownedKeys(rel2))
	must.Eq(t, &structs.ReleaseMultiregion{Regions: []string{"r1", "r2"}, GlobalRegion: "r1"}, rel2.Spec.Multiregion)

	actions := map[string]string{}
	for _, ch := range resp.Regions[1].Changes {
		actions[ch.Type+"/"+ch.Name] = ch.Action + "@" + ch.Region
	}
	must.Eq(t, "remote@r1", actions["namespace/mr-ns"])
	must.Eq(t, "remote@r1", actions["acl-policy/mr-read"])
	must.Eq(t, "adopt@r1", actions["job/mr-multi"])
	must.Eq(t, "create@", actions["job/mr-web"])

	// The multiregion job was registered once, by r1, with the same version
	// in both regions; the regular job exists in both regions.
	j1, err := f.s1.State().JobByID(nil, "default", "mr-multi")
	must.NoError(t, err)
	j2, err := f.s2.State().JobByID(nil, "default", "mr-multi")
	must.NoError(t, err)
	must.NotNil(t, j1)
	must.NotNil(t, j2)
	must.Eq(t, j1.Version, j2.Version)
	must.Eq(t, "r2", j2.Region)
	for _, s := range []*Server{f.s1, f.s2} {
		j, err := s.State().JobByID(nil, "mr-ns", "mr-web")
		must.NoError(t, err)
		must.NotNil(t, j)
		must.Eq(t, s.Region(), j.Region)
	}

	// An upgrade of r2 alone keeps the division of work of the install.
	resp, err = f.apply("r2", "mr", &structs.ReleaseApplyRequest{Resources: testMultiregionPack()})
	must.NoError(t, err)
	must.Eq(t, 2, resp.Revision)
	rel2 = f.get("r2", "mr")
	must.Eq(t, []string{"job/default/mr-multi", "job/mr-ns/mr-web", "variable/default/config/mr"}, ownedKeys(rel2))
	must.NotNil(t, rel2.Spec.Multiregion)

	// Partial success: a job owned by another release of r2 fails the
	// install there, the release of r1 is kept.
	_, err = f.apply("r2", "blocker", &structs.ReleaseApplyRequest{
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("mr-dup")}},
	})
	must.NoError(t, err)
	resp, err = f.apply("r2", "mr2", &structs.ReleaseApplyRequest{
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("mr-dup")}},
		Regions:   []string{"r1", "r2"},
	})
	must.NoError(t, err)
	must.Len(t, 2, resp.Regions)
	must.Eq(t, "", resp.Regions[0].Error)
	must.StrContains(t, resp.Regions[1].Error, "belong to other releases")
	must.Eq(t, structs.ReleasePhaseDeployed, f.get("r1", "mr2").Status.Phase)
	// The conflict is found before the release of r2 is created.
	must.Nil(t, f.get("r2", "mr2"))

	// A failure in every region is an error.
	_, err = f.apply("r2", "mr3", &structs.ReleaseApplyRequest{
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("mr-dup")}},
		Regions:   []string{"r2"},
	})
	must.ErrorContains(t, err, "failed in every region")

	// Uninstalling the release of r2 keeps the global resources, which the
	// release of r1 owns.
	f.uninstall("r2", "mr")
	must.Nil(t, f.get("r2", "mr"))
	ns, err := f.s1.State().NamespaceByName(nil, "mr-ns")
	must.NoError(t, err)
	must.NotNil(t, ns)
	pol, err := f.s1.State().ACLPolicyByName(nil, "mr-read")
	must.NoError(t, err)
	must.NotNil(t, pol)
	j, err := f.s2.State().JobByID(nil, "mr-ns", "mr-web")
	must.NoError(t, err)
	must.Nil(t, j)
	j, err = f.s1.State().JobByID(nil, "mr-ns", "mr-web")
	must.NoError(t, err)
	must.NotNil(t, j)

	// Uninstalling the release of r1 deletes the global resources.
	f.uninstall("r1", "mr")
	ns, err = f.s1.State().NamespaceByName(nil, "mr-ns")
	must.NoError(t, err)
	must.Nil(t, ns)
}
