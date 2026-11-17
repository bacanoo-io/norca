// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// writeNamedPack writes a pack with a name and a README to dir.
func writeNamedPack(t *testing.T, dir, name, version, readme string) {
	t.Helper()
	files := map[string]string{
		"metadata.hcl":                     "app {\n  url = \"https://example.com\"\n}\n\npack {\n  name    = \"" + name + "\"\n  version = \"" + version + "\"\n}\n",
		"README.md":                        readme,
		"variables.hcl":                    "variable \"greeting\" {\n  type    = string\n  default = \"hi\"\n}\n",
		"templates/" + name + ".nomad.tpl": "job \"" + name + "\" {\n  group \"g\" {\n    task \"t\" {\n      driver = \"raw_exec\"\n      config {\n        command = \"echo\"\n      }\n    }\n  }\n}\n",
	}
	for file, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(file))
		must.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

// The pack and the version of Packs.Detail and Packs.Deploy name a pack of
// the registry; they are never a path (security review finding SEC-4). With
// "../.." in them a caller chose any directory of the leader: a reader of
// the catalog read files outside of the cache, and a caller with release
// write deployed the pack of another registry as a server-rendered release
// of the approved one.
func TestPacks_PackAndVersionAreNotPaths(t *testing.T) {
	ci.Parallel(t)

	var mu sync.Mutex
	var rendered []*api.ReleasePack
	convert := func(name string, req *api.ReleaseApplyRequest, namespace string) (*structs.ReleaseApplyRequest, error) {
		mu.Lock()
		defer mu.Unlock()
		rendered = append(rendered, req.Pack)
		return &structs.ReleaseApplyRequest{Name: name, Values: req.Values}, nil
	}
	renders := func() []*api.ReleasePack {
		mu.Lock()
		defer mu.Unlock()
		return append([]*api.ReleasePack(nil), rendered...)
	}
	s, root, cleanup := TestACLServer(t, func(c *Config) { c.ReleaseFromRendered = convert })
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	codec := rpcClient(t, s)

	upsert := func(reg *PackRegistry) (*PackRegistry, error) {
		var resp PackRegistryResponse
		err := msgpackrpc.CallWithCodec(codec, "Packs.Upsert", &PackRegistryUpsertRequest{
			Registry:     reg,
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID},
		}, &resp)
		return resp.Registry, err
	}
	approvedSource, otherSource := t.TempDir(), t.TempDir()
	writeNamedPack(t, approvedSource, "hello", "1.0.0", "the approved pack")
	writeNamedPack(t, otherSource, "evil", "6.6.6", "the pack of another registry")
	for name, source := range map[string]string{"approved": approvedSource, "other": otherSource} {
		reg, err := upsert(&PackRegistry{Name: name, Spec: &PackRegistrySpec{Type: "dir", Source: source, SyncInterval: "5m"}})
		must.NoError(t, err)
		must.Eq(t, PackRegistryPhaseReady, reg.Status.Phase, must.Sprint(reg.Status.Message))
	}
	cache, err := s.releases.packCachePath()
	must.NoError(t, err)

	// A pack in a directory of the server that is no cache of a registry,
	// named like a pack directory.
	const canary = "outside-canary-5c1d"
	outside := t.TempDir()
	writeNamedPack(t, filepath.Join(outside, "secret@x"), "secret", "1.0.0", canary)
	outsidePack, err := filepath.Rel(filepath.Join(cache, "approved", "x"), filepath.Join(outside, "secret"))
	must.NoError(t, err)

	reader := mock.CreatePolicyAndToken(t, s.State(), 1001, "release-read",
		`namespace "default" { object "packs.norca.io/Release" { capabilities = ["read"] } }`)
	writer := mock.CreatePolicyAndToken(t, s.State(), 1003, "release-write",
		`namespace "default" { object "packs.norca.io/Release" { capabilities = ["read", "write"] } }`)

	detail := func(token, registry, pack, version string) (*PackDetailResponse, error) {
		var resp PackDetailRPCResponse
		err := msgpackrpc.CallWithCodec(codec, "Packs.Detail", &PackDetailRequest{
			Registry: registry, Pack: pack, Version: version,
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: token},
		}, &resp)
		return resp.Detail, err
	}
	deploy := func(token, registry, pack, version string) error {
		return msgpackrpc.CallWithCodec(codec, "Packs.Deploy", &PackDeployRPCRequest{
			Name:         "app",
			Deploy:       &PackDeployRequest{Registry: registry, Pack: pack, Version: version},
			DryRun:       true,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token},
		}, new(structs.ReleaseApplyResponse))
	}

	attacks := []struct{ name, pack, version string }{
		{"the pack of another registry", "../../other/latest/evil", "latest"},
		{"the pack of another registry by the version", "evil", "../../other/latest"},
		{"a directory outside of the cache", outsidePack, "x"},
		{"a parent directory", "..", "latest"},
		{"an absolute path", filepath.Join(outside, "secret"), "x"},
		{"the pack of another registry, Windows separators", `..\..\other\latest\evil`, "latest"},
		{"a drive", `C:\packs\evil`, "latest"},
		{"a version with a separator", "hello", `1.0.0\..`},
	}
	for _, a := range attacks {
		got, err := detail(reader.SecretID, "approved", a.pack, a.version)
		readme := ""
		if got != nil {
			readme = got.Readme
		}
		must.ErrorContains(t, err, "invalid pack", must.Sprintf("%s: the detail has the README %q", a.name, readme))
		must.StrNotContains(t, err.Error(), canary)

		err = deploy(writer.SecretID, "approved", a.pack, a.version)
		must.ErrorContains(t, err, "invalid pack", must.Sprintf("%s: deploy", a.name))
	}
	must.SliceEmpty(t, renders(), must.Sprint("a pack was rendered from a path of the request"))

	// A pack the registry does not list is not one of its packs, even with
	// a directory of that name in its cache.
	writeNamedPack(t, filepath.Join(cache, "approved", "latest", "planted@latest"), "planted", "1.0.0", canary)
	_, err = detail(reader.SecretID, "approved", "planted", "")
	must.ErrorContains(t, err, "not found")
	must.ErrorContains(t, deploy(writer.SecretID, "approved", "planted", "latest"), "not found")
	_, err = detail(reader.SecretID, "approved", "evil", "")
	must.ErrorContains(t, err, "not found")
	must.SliceEmpty(t, renders())

	// The packs of the registry are served as before: by name, by their
	// version and by the ref of the registry.
	for _, version := range []string{"", "1.0.0", "latest"} {
		got, err := detail(reader.SecretID, "approved", "hello", version)
		must.NoError(t, err, must.Sprint(version))
		must.Eq(t, "hello", got.Pack.Name)
		must.Eq(t, "1.0.0", got.Pack.Version)
		must.Eq(t, "the approved pack", got.Readme)
		must.NoError(t, deploy(writer.SecretID, "approved", "hello", version), must.Sprint(version))
	}
	got := renders()
	must.Len(t, 3, got)
	for _, pack := range got {
		must.Eq(t, "hello", pack.Name)
		must.Eq(t, "approved", pack.Registry)
		must.Eq(t, "1.0.0", pack.Version)
		must.Eq(t, filepath.Join(cache, "approved", "latest", "hello@latest"), pack.Path)
	}

	// The default ref of a registry is the name of a directory of its cache
	// too: one that leaves the cache is refused when the registry is saved.
	for _, ref := range []string{"../../x", "..", `..\x`, "a/b", "-rf"} {
		_, err := upsert(&PackRegistry{Name: "badref", Spec: &PackRegistrySpec{Type: "dir", Source: approvedSource, DefaultRef: ref}})
		must.ErrorContains(t, err, "invalid default ref", must.Sprint(ref))
	}
	badref, _, err := s.GetPackRegistry(&structs.GenericRequest{
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID}}, "badref")
	must.NoError(t, err)
	must.Nil(t, badref)
}
