// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/shoenig/test/must"

	ct "github.com/hashicorp/nomad/pack/internal/cli/testhelper"
	"github.com/hashicorp/nomad/pack/internal/pkg/caching"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci/ocitest"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
	"github.com/hashicorp/nomad/pack/internal/runner/job"
)

// The tests in this file exercise the supply chain features with the redis
// pack of the community registry (vendored in fixtures/community, MPL-2.0).
// They set environment variables and therefore do not run in parallel.

const communityRedis = "community/packs/redis"

// redisVars avoid the Consul service with a Connect sidecar, which the test
// agent can not provide.
var redisVars = []string{"--var=register_consul_service=false"}

// isolateSupplyChain points the pack configuration, the cache and the key
// password at test locations.
func isolateSupplyChain(t *testing.T) (configFile, cacheDir string) {
	t.Helper()
	configFile = filepath.Join(t.TempDir(), "config.hcl")
	cacheDir = t.TempDir()
	t.Setenv(packconfig.EnvConfigFile, configFile)
	t.Setenv(caching.EnvCacheDir, cacheDir)
	t.Setenv(EnvKeyPassword, "test-password")
	t.Setenv(EnvLockMode, "")
	return configFile, cacheDir
}

func expectExit(t *testing.T, r PackCommandResult, code int) {
	t.Helper()
	must.Eq(t, code, r.exitCode, must.Sprintf("stdout:\n%s\nstderr:\n%s", r.cmdOut.String(), r.cmdErr.String()))
}

// TestCLI_CommunityRedis renders, plans and runs the unmodified community
// redis pack against a Norca agent. The agent has no docker driver, so the
// job is registered (run --detach) but not placed.
func TestCLI_CommunityRedis(t *testing.T) {
	isolateSupplyChain(t)
	packDir := testfixture.Clone(t, communityRedis)

	r := runPackCmd(t, append([]string{"render", packDir}, redisVars...))
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), `job "redis"`)
	must.StrContains(t, r.cmdOut.String(), `driver = "docker"`)
	must.StrContains(t, r.cmdOut.String(), `image = "redis:latest"`)

	ct.HTTPTest(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		r := runTestPackCmd(t, s, append([]string{"plan", packDir}, redisVars...))
		must.StrContains(t, r.cmdOut.String(), "Plan succeeded", must.Sprintf("output: %s", r.cmdOut.String()))
		must.Eq(t, 1, r.exitCode) // changes would be made

		r = runTestPackCmd(t, s, append([]string{"run", packDir, "--detach"}, redisVars...))
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), "Pack successfully deployed")

		client, err := ct.NewTestClient(s)
		must.NoError(t, err)
		j, _, err := client.Jobs().Info("redis", &api.QueryOptions{})
		must.NoError(t, err)
		must.Eq(t, "redis", j.Meta[job.PackNameKey])
		must.Eq(t, "docker", j.TaskGroups[0].Tasks[0].Driver)
		// Local packs carry no OCI provenance.
		must.MapNotContainsKey(t, j.Meta, job.PackDigestKey)

		r = runTestPackCmd(t, s, []string{"destroy", packDir})
		// The exit code reflects the cancelled deployment monitor, not the destroy.
		must.StrContains(t, r.cmdOut.String(), `Pack "redis" destroyed`)
	})
}

// TestCLI_CommunityRedis_Network fetches the current redis pack from the
// community registry with git and renders it. It needs network access and
// is enabled by NORCA_PACK_COMMUNITY_TESTS=1.
func TestCLI_CommunityRedis_Network(t *testing.T) {
	if os.Getenv("NORCA_PACK_COMMUNITY_TESTS") != "1" {
		t.Skip("set NORCA_PACK_COMMUNITY_TESTS=1 to fetch the community registry")
	}
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	isolateSupplyChain(t)

	r := runPackCmd(t, []string{"registry", "add", "community", "github.com/hashicorp/nomad-pack-community-registry", "--target=redis"})
	expectExit(t, r, 0)
	r = runPackCmd(t, append([]string{"render", "redis", "--registry=community"}, redisVars...))
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), `job "redis"`)
}

// TestCLI_SupplyChain covers the OCI workflow end to end with the redis pack:
// key generation, registry configuration, push, signature enforcement,
// sign, verify, download on use, provenance in the job meta, bundles and
// air-gapped use from the cache.
func TestCLI_SupplyChain(t *testing.T) {
	configFile, cacheDir := isolateSupplyChain(t)
	keyDir := t.TempDir()
	packDir := testfixture.Clone(t, communityRedis)

	reg := ocitest.New()
	srv := reg.Start()
	defer srv.Close()
	url := "oci://" + srv.Host() + "/packs"

	r := runPackCmd(t, []string{"key", "generate", "--output-prefix=" + filepath.Join(keyDir, "cosign")})
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), "Key ID: sha256:")
	pubKey := filepath.Join(keyDir, "cosign.pub")
	privKey := filepath.Join(keyDir, "cosign.key")

	// Refuses to overwrite keys.
	r = runPackCmd(t, []string{"key", "generate", "--output-prefix=" + filepath.Join(keyDir, "cosign")})
	expectExit(t, r, 1)

	r = runPackCmd(t, []string{"registry", "add", "internal", url, "--plain-http",
		"--trust-key=" + pubKey, "--require-signature"})
	expectExit(t, r, 0)
	conf, err := packconfig.Load(configFile)
	must.NoError(t, err)
	must.True(t, conf.TrustPolicy("internal").RequireSignature)

	r = runPackCmd(t, []string{"registry", "list"})
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), url)

	r = runPackCmd(t, []string{"push", packDir, "--registry=internal"})
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), "Pushed "+url+"/redis:0.2.1")
	must.Eq(t, []string{"0.2.1"}, reg.Tags("packs/redis"))

	// Unsigned packs are rejected by the trust policy.
	r = runPackCmd(t, append([]string{"render", "redis", "--registry=internal"}, redisVars...))
	expectExit(t, r, 1)
	must.StrContains(t, r.cmdOut.String()+r.cmdErr.String(), "not signed")

	r = runPackCmd(t, []string{"verify", "--registry=internal", "redis:0.2.1"})
	expectExit(t, r, 1)

	r = runPackCmd(t, []string{"sign", "--key=" + privKey, "--registry=internal", "redis:0.2.1"})
	expectExit(t, r, 0)

	r = runPackCmd(t, []string{"verify", "--registry=internal", "redis:0.2.1"})
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), "Signature verified")

	// Downloaded on first use, newest version, verified.
	r = runPackCmd(t, append([]string{"render", "redis", "--registry=internal"}, redisVars...))
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), `job "redis"`)
	must.StrContains(t, r.cmdOut.String(), "signed by key sha256:")
	cached := filepath.Join(cacheDir, "internal", "0.2.1", "redis@0.2.1")
	_, err = os.Stat(filepath.Join(cached, packlock.OriginFileName))
	must.NoError(t, err)

	// Plan and run record the provenance in the job meta.
	ct.HTTPTest(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		r := runTestPackCmd(t, s, append([]string{"plan", "redis", "--registry=internal", "--ref=0.2.1"}, redisVars...))
		must.Eq(t, 1, r.exitCode, must.Sprintf("output: %s", r.cmdOut.String()))

		r = runTestPackCmd(t, s, append([]string{"run", "redis", "--registry=internal", "--ref=0.2.1", "--detach"}, redisVars...))
		expectExit(t, r, 0)
		client, err := ct.NewTestClient(s)
		must.NoError(t, err)
		j, _, err := client.Jobs().Info("redis", &api.QueryOptions{})
		must.NoError(t, err)
		must.StrHasPrefix(t, "sha256:", j.Meta[job.PackDigestKey])
		must.StrHasPrefix(t, "sha256:", j.Meta[job.PackSignerKey])
		must.Eq(t, "internal", j.Meta[job.PackRegistryKey])
		must.Eq(t, "0.2.1", j.Meta[job.PackRefKey])
	})

	// A modified cached pack is rejected.
	tpl := filepath.Join(cached, "templates", "redis.nomad.tpl")
	orig, err := os.ReadFile(tpl)
	must.NoError(t, err)
	must.NoError(t, os.WriteFile(tpl, []byte(string(orig)+"\n# tampered\n"), 0o644))
	r = runPackCmd(t, append([]string{"render", "redis", "--registry=internal", "--ref=0.2.1"}, redisVars...))
	expectExit(t, r, 1)
	must.StrContains(t, r.cmdOut.String()+r.cmdErr.String(), "modified after download")
	must.NoError(t, os.WriteFile(tpl, orig, 0o644))

	// Bundle the signed pack, then work without the registry.
	bundleFile := filepath.Join(t.TempDir(), "packs.tar")
	r = runPackCmd(t, []string{"bundle", "create", "--output=" + bundleFile, "--registry=internal", "redis:0.2.1"})
	expectExit(t, r, 0)
	r = runPackCmd(t, []string{"bundle", "inspect", bundleFile})
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), "redis")
	must.StrContains(t, r.cmdOut.String(), "true")

	srv.Close()

	// The bundle is verified with the trust policy of the target name: a
	// policy requiring signatures by another key rejects it.
	otherKeyDir := t.TempDir()
	r = runPackCmd(t, []string{"key", "generate", "--output-prefix=" + filepath.Join(otherKeyDir, "other")})
	expectExit(t, r, 0)
	must.NoError(t, os.WriteFile(configFile, []byte(`
trust {
  require_signature = true
  public_keys       = ["`+filepath.ToSlash(filepath.Join(otherKeyDir, "other.pub"))+`"]
}
`), 0o600))
	r = runPackCmd(t, []string{"bundle", "import", bundleFile, "--cache-as=offline"})
	expectExit(t, r, 1)

	must.NoError(t, os.WriteFile(configFile, []byte(`
trust {
  require_signature = true
  public_keys       = ["`+filepath.ToSlash(pubKey)+`"]
}
`), 0o600))
	r = runPackCmd(t, []string{"bundle", "import", bundleFile, "--cache-as=offline"})
	expectExit(t, r, 0)
	r = runPackCmd(t, append([]string{"render", "redis", "--registry=offline", "--ref=0.2.1"}, redisVars...))
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), `job "redis"`)

	// Push the bundle into a new internal registry and use it from there.
	internal := ocitest.New().Start()
	defer internal.Close()
	mirror := "oci://" + internal.Host() + "/mirror"
	r = runPackCmd(t, []string{"registry", "add", "mirror", mirror, "--plain-http", "--trust-key=" + pubKey, "--require-signature"})
	expectExit(t, r, 0)
	r = runPackCmd(t, []string{"bundle", "import", bundleFile, "--registry=mirror"})
	expectExit(t, r, 0)
	r = runPackCmd(t, append([]string{"render", "redis", "--registry=mirror"}, redisVars...))
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), `job "redis"`)

	r = runPackCmd(t, []string{"registry", "delete", "mirror"})
	expectExit(t, r, 0)
	conf, err = packconfig.Load(configFile)
	must.NoError(t, err)
	must.Nil(t, conf.Registry("mirror"))
}

// TestCLI_Lock covers the --lock modes of render with a local pack.
func TestCLI_Lock(t *testing.T) {
	isolateSupplyChain(t)
	packDir := testfixture.Clone(t, "v2/test_registry/packs/my_alias_test")

	r := runPackCmd(t, []string{"render", packDir, "--lock=require"})
	expectExit(t, r, 1)
	must.StrContains(t, r.cmdOut.String()+r.cmdErr.String(), "has no pack.lock")

	r = runPackCmd(t, []string{"render", packDir, "--lock=update"})
	expectExit(t, r, 0)
	_, err := os.Stat(packlock.Path(packDir))
	must.NoError(t, err)

	r = runPackCmd(t, []string{"render", packDir, "--lock=require"})
	expectExit(t, r, 0)

	// Changing a vendored dependency breaks the verification.
	dep := filepath.Join(packDir, "deps", "child1", "templates", "extra.tpl")
	must.NoError(t, os.WriteFile(dep, []byte("[[/* x */]]"), 0o644))
	r = runPackCmd(t, []string{"render", packDir})
	expectExit(t, r, 1)
	must.StrContains(t, r.cmdOut.String()+r.cmdErr.String(), "child1")

	r = runPackCmd(t, []string{"render", packDir, "--lock=ignore"})
	expectExit(t, r, 0)

	// Pushing a pack whose lock does not match is refused.
	r = runPackCmd(t, []string{"push", packDir, "oci://localhost:1/packs/deps_test"})
	expectExit(t, r, 1)
	must.StrContains(t, r.cmdOut.String()+r.cmdErr.String(), "pack.lock")
}
