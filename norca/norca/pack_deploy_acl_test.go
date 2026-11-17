// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// Packs.Deploy authorizes its caller before it looks at the registry, syncs
// its cache or renders (review finding SRV-7): a caller without release
// write in the namespace learns nothing about registries and packs and makes
// the leader do no work.
func TestPacks_DeployAuthorizedBeforeRender(t *testing.T) {
	ci.Parallel(t)

	var converted atomic.Int64
	convert := func(name string, rendered *api.ReleaseApplyRequest, namespace string) (*structs.ReleaseApplyRequest, error) {
		converted.Add(1)
		return &structs.ReleaseApplyRequest{Name: name, Values: rendered.Values}, nil
	}
	leader, root, cleanupLeader := TestACLServer(t, func(c *Config) {
		c.ReleaseFromRendered = convert
	})
	defer cleanupLeader()
	testutil.WaitForLeader(t, leader.RPC)
	follower, cleanupFollower := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 0
		c.ACLEnabled = true
		c.ReleaseFromRendered = convert
	})
	defer cleanupFollower()
	TestJoin(t, leader, follower)
	testutil.WaitForLeader(t, follower.RPC)
	must.True(t, leader.IsLeader())

	source := t.TempDir()
	writeDirPack(t, source, "1.0.0")
	var reg PackRegistryResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, leader), "Packs.Upsert", &PackRegistryUpsertRequest{
		Registry:     &PackRegistry{Name: "local", Spec: &PackRegistrySpec{Type: "dir", Source: source, SyncInterval: "5m"}},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID},
	}, &reg))
	must.Eq(t, PackRegistryPhaseReady, reg.Registry.Status.Phase)

	// The tokens are the leader's, which answers what the follower forwards.
	otherNamespace := mock.CreatePolicyAndToken(t, leader.State(), 1001, "release-dev",
		`namespace "dev" { object "packs.norca.io/Release" { capabilities = ["read", "write"] } }`)
	reader := mock.CreatePolicyAndToken(t, leader.State(), 1003, "release-read",
		`namespace "default" { object "packs.norca.io/Release" { capabilities = ["read"] } }`)
	writer := mock.CreatePolicyAndToken(t, leader.State(), 1005, "release-write",
		`namespace "default" { object "packs.norca.io/Release" { capabilities = ["read", "write"] } }`)

	// Without the cache a render would sync the registry again and record a
	// new generation.
	cache, err := leader.releases.packCachePath()
	must.NoError(t, err)
	generation := func() int64 {
		got, _, err := leader.GetPackRegistry(&structs.GenericRequest{
			QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID}}, "local")
		must.NoError(t, err)
		return got.Status.SyncGeneration
	}
	before := generation()
	must.NoError(t, os.RemoveAll(filepath.Join(cache, "local")))

	deploy := func(s *Server, token, registry, pack string) error {
		codec := rpcClient(t, s)
		defer codec.Close()
		return msgpackrpc.CallWithCodec(codec, "Packs.Deploy", &PackDeployRPCRequest{
			Name:         "hello",
			Deploy:       &PackDeployRequest{Registry: registry, Pack: pack},
			DryRun:       true,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token},
		}, new(structs.ReleaseApplyResponse))
	}

	for name, s := range map[string]*Server{"leader": leader, "follower": follower} {
		for who, token := range map[string]string{
			"anonymous":                         "",
			"release write in another namespace": otherNamespace.SecretID,
			"release read":                      reader.SecretID,
		} {
			// The same answer whether the registry and the pack exist or not.
			for _, target := range [][2]string{{"local", "hello"}, {"local", "nope"}, {"nope", "hello"}} {
				err := deploy(s, token, target[0], target[1])
				must.ErrorContains(t, err, structs.ErrPermissionDenied.Error(),
					must.Sprintf("%s, %s, %s/%s", name, who, target[0], target[1]))
				must.StrNotContains(t, err.Error(), "not found")
			}
		}
	}
	must.Eq(t, 0, converted.Load(), must.Sprint("a pack was rendered for a caller without release write"))
	must.Eq(t, before, generation(), must.Sprint("the registry was synced for a caller without release write"))

	// Release write in the namespace renders and plans.
	for name, s := range map[string]*Server{"leader": leader, "follower": follower} {
		must.NoError(t, deploy(s, writer.SecretID, "local", "hello"), must.Sprint(name))
		must.ErrorContains(t, deploy(s, writer.SecretID, "local", "nope"), "not found")
	}
	must.Eq(t, 2, converted.Load())
	must.NoError(t, deploy(leader, root.SecretID, "local", "hello"))
}
