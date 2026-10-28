// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// writeDirPack writes a pack to dir, a source of a "dir" registry.
func writeDirPack(t *testing.T, dir, version string) {
	files := map[string]string{
		"metadata.hcl":              "app {\n  url = \"https://example.com\"\n}\n\npack {\n  name    = \"hello\"\n  version = \"" + version + "\"\n}\n",
		"variables.hcl":             "variable \"greeting\" {\n  type    = string\n  default = \"hi\"\n  ui {\n    tab = \"general\"\n  }\n}\n",
		"templates/hello.nomad.tpl": "job \"hello\" {\n  group \"g\" {\n    task \"t\" {\n      driver = \"raw_exec\"\n      config {\n        command = \"echo\"\n        args    = [[ var \"greeting\" . | toJson ]]\n      }\n    }\n  }\n}\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		must.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

func TestPacks_ForwardedToLeader(t *testing.T) {
	ci.Parallel(t)

	var converted int
	convert := func(name string, rendered *api.ReleaseApplyRequest, namespace string) (*structs.ReleaseApplyRequest, error) {
		converted++
		return &structs.ReleaseApplyRequest{Name: name, Values: rendered.Values}, nil
	}
	s1, cleanup1 := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		c.ReleaseFromRendered = convert
	})
	defer cleanup1()
	s2, cleanup2 := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 2
		c.ReleaseFromRendered = convert
	})
	defer cleanup2()
	TestJoin(t, s1, s2)
	testutil.WaitForLeader(t, s1.RPC)
	testutil.WaitForLeader(t, s2.RPC)

	leader, follower := s1, s2
	if !s1.IsLeader() {
		leader, follower = s2, s1
	}
	must.False(t, follower.IsLeader(), must.Sprint("one raft cluster"))
	must.Len(t, 2, leader.raft.GetConfiguration().Configuration().Servers)
	codec := rpcClient(t, follower)
	write := structs.WriteRequest{Region: "global"}
	query := structs.QueryOptions{Region: "global"}

	source := t.TempDir()
	writeDirPack(t, source, "1.0.0")

	// Writes through the follower run on the leader.
	var reg PackRegistryResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Upsert", &PackRegistryUpsertRequest{
		Registry:     &PackRegistry{Name: "local", Spec: &PackRegistrySpec{Type: "dir", Source: source, SyncInterval: "5m"}},
		WriteRequest: write,
	}, &reg))
	must.Eq(t, PackRegistryPhaseReady, reg.Registry.Status.Phase)
	must.Eq(t, int64(1), reg.Registry.Status.SyncGeneration)
	must.Eq(t, 1, reg.Registry.Status.PackCount)

	leaderCache, err := leader.releases.packCachePath()
	must.NoError(t, err)
	must.Eq(t, int64(1), packCacheGeneration(leaderCache, "local"))

	var list PackRegistryListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Catalog", &PackRegistryListRequest{QueryOptions: query}, &list))
	must.Len(t, 1, list.Packs)

	// The detail comes from the cache of the leader.
	var detail PackDetailRPCResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Detail", &PackDetailRequest{
		Registry: "local", Pack: "hello", QueryOptions: query,
	}, &detail))
	must.Eq(t, "1.0.0", detail.Detail.Pack.Version)
	props := detail.Detail.UIFormSchema["properties"].(map[string]any)
	must.MapContainsKey(t, props, "greeting")

	// A leader without the cache (e.g. a new leader) fetches the registry
	// again and records a new generation.
	must.NoError(t, os.RemoveAll(filepath.Join(leaderCache, "local")))
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Detail", &PackDetailRequest{
		Registry: "local", Pack: "hello", QueryOptions: query,
	}, &detail))
	must.Eq(t, int64(2), packCacheGeneration(leaderCache, "local"))
	got, _, err := leader.GetPackRegistry(&structs.GenericRequest{QueryOptions: query}, "local")
	must.NoError(t, err)
	must.Eq(t, int64(2), got.Status.SyncGeneration)

	// Renders run on the leader, as server-rendered releases.
	var applied structs.ReleaseApplyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Deploy", &PackDeployRPCRequest{
		Name:         "hello",
		Deploy:       &PackDeployRequest{Registry: "local", Pack: "hello", Values: map[string]any{"greeting": "hey"}},
		DryRun:       true,
		WriteRequest: write,
	}, &applied))
	must.Eq(t, 1, converted, must.Sprint("converted on the leader only"))

	// A failed sync keeps the packs and the cache of the last good one.
	must.NoError(t, os.RemoveAll(source))
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Sync", &PackRegistryNameRequest{Name: "local", WriteRequest: write}, &reg))
	must.Eq(t, PackRegistryPhaseError, reg.Registry.Status.Phase)
	must.Eq(t, 1, reg.Registry.Status.PackCount)
	must.Eq(t, int64(2), reg.Registry.Status.SyncGeneration)
	must.NotEq(t, "", reg.Registry.Status.LastGoodSyncAt)
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Detail", &PackDetailRequest{
		Registry: "local", Pack: "hello", QueryOptions: query,
	}, &detail), must.Sprint("the cache still serves the pack"))

	// The leader syncs registries whose interval passed.
	writeDirPack(t, source, "1.1.0")
	leader.syncDuePackRegistries(time.Now())
	got, _, _ = leader.GetPackRegistry(&structs.GenericRequest{QueryOptions: query}, "local")
	must.Eq(t, PackRegistryPhaseError, got.Status.Phase, must.Sprint("not due yet"))
	leader.syncDuePackRegistries(time.Now().Add(6 * time.Minute))
	got, _, _ = leader.GetPackRegistry(&structs.GenericRequest{QueryOptions: query}, "local")
	must.Eq(t, PackRegistryPhaseReady, got.Status.Phase)
	must.Eq(t, int64(3), got.Status.SyncGeneration)
	must.Eq(t, "1.1.0", got.Status.Packs[0].Version)

	// Intervals under a minute are rejected.
	err = msgpackrpc.CallWithCodec(codec, "Packs.Upsert", &PackRegistryUpsertRequest{
		Registry:     &PackRegistry{Name: "fast", Spec: &PackRegistrySpec{Type: "dir", Source: source, SyncInterval: "10s"}},
		WriteRequest: write,
	}, &reg)
	must.ErrorContains(t, err, "at least 1m0s")

	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Delete", &PackRegistryNameRequest{Name: "local", WriteRequest: write}, &reg))
	_, err = os.Stat(filepath.Join(leaderCache, "local"))
	must.True(t, os.IsNotExist(err))
}
