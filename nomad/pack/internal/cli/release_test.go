// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/shoenig/test/must"

	ct "github.com/hashicorp/nomad/pack/internal/cli/testhelper"
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
	"github.com/hashicorp/nomad/pack/internal/runner/job"
)

// TestCLI_ReleaseCommunityRedis installs the unmodified community redis pack
// as a release, upgrades it, rolls it back and uninstalls it.
func TestCLI_ReleaseCommunityRedis(t *testing.T) {
	isolateSupplyChain(t)
	packDir := testfixture.Clone(t, communityRedis)

	ct.HTTPTest(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		client, err := ct.NewTestClient(s)
		must.NoError(t, err)

		// Plan: everything is created.
		r := runTestPackCmd(t, s, append([]string{"plan", packDir}, redisVars...))
		must.StrContains(t, r.cmdOut.String(), `Release "redis", revision 1`)
		must.Eq(t, 1, r.exitCode, must.Sprintf("output: %s", r.cmdOut.String()))

		r = runTestPackCmd(t, s, append([]string{"run", packDir, "--detach"}, redisVars...))
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), `Release "redis" deployed at revision 1`)
		must.StrContains(t, r.cmdOut.String(), "Pack successfully deployed")

		j, _, err := client.Jobs().Info("redis", nil)
		must.NoError(t, err)
		must.Eq(t, "redis", j.Meta[job.PackNameKey])
		must.Eq(t, "redis", j.Meta[api.ReleaseMetaRelease])

		rel, _, err := client.Releases().Info("redis", nil)
		must.NoError(t, err)
		must.Eq(t, "0.2.1", rel.Spec.Pack.Version)
		must.Eq(t, "", rel.Spec.Pack.Source, must.Sprint("a local directory has no source"))
		must.Eq(t, api.ReleasePhaseDeployed, rel.Status.Phase)

		// Upgrade with another image; the variable is recorded.
		r = runTestPackCmd(t, s, append([]string{"upgrade", packDir, "--detach", "--var=image=redis:7"}, redisVars...))
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), `deployed at revision 2`)
		j, _, err = client.Jobs().Info("redis", nil)
		must.NoError(t, err)
		must.Eq(t, "redis:7", j.TaskGroups[0].Tasks[0].Config["image"])
		rev, _, err := client.Releases().Revision("redis", 2, nil)
		must.NoError(t, err)
		must.Eq[any](t, "redis:7", rev.Spec.Values["image"])

		r = runTestPackCmd(t, s, []string{"history", "redis"})
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), "superseded")
		must.StrContains(t, r.cmdOut.String(), "upgrade")

		// Roll back to revision 1: a new revision 3 with the old image.
		r = runTestPackCmd(t, s, []string{"rollback", "redis", "1", "--detach"})
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), "revision 3")
		j, _, err = client.Jobs().Info("redis", nil)
		must.NoError(t, err)
		must.Eq(t, "redis:latest", j.TaskGroups[0].Tasks[0].Config["image"])

		r = runTestPackCmd(t, s, []string{"status", "redis"})
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), "Deployed revision")

		r = runTestPackCmd(t, s, []string{"diff", "redis", "--live"})
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), "matches revision 3")

		// A job changed by hand drifts.
		j.TaskGroups[0].Count = pointerOf(2)
		_, _, err = client.Jobs().Register(j, nil)
		must.NoError(t, err)
		r = runTestPackCmd(t, s, []string{"diff", "redis", "--live"})
		expectExit(t, r, 2)
		must.StrContains(t, r.cmdOut.String(), "modified")

		// destroy uninstalls the release.
		r = runTestPackCmd(t, s, []string{"destroy", packDir})
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), `Release "redis" uninstalled`)
		_, _, err = client.Jobs().Info("redis", nil)
		must.ErrorContains(t, err, "404")
		_, _, err = client.Releases().Info("redis", nil)
		must.ErrorContains(t, err, "404")
	})
}

func pointerOf[T any](v T) *T { return &v }

// testRegistrySeq makes the names of test registries unique: parallel tests
// created in the same millisecond used to collide.
var testRegistrySeq atomic.Int64

func uniqueTestRegistryName() string {
	return fmt.Sprintf("test-%v-%d", time.Now().UnixMilli(), testRegistrySeq.Add(1))
}

// writeExtendedPack writes a pack with a namespace, a quota, an ACL policy,
// a Variables entry with a secret item and two jobs.
func writeExtendedPack(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "team")
	files := map[string]string{
		"metadata.hcl": `
app {
  url = "https://example.com"
}
pack {
  name        = "team"
  description = "A team environment"
  version     = "1.0.0"
  requires {
    norca    = ">= 0.1.0"
    features = ["releases", "quotas"]
  }
}
`,
		"variables.hcl": `
variable "team" {
  type    = string
  default = "blue"
}
`,
		"templates/team.namespace.hcl.tpl": `
name        = "[[ var "team" . ]]"
description = "Namespace of team [[ var "team" . ]]"
quota       = "[[ var "team" . ]]-quota"
`,
		"templates/team.quota.hcl.tpl": `
name = "[[ var "team" . ]]-quota"
limit {
  region = "global"
  region_limit {
    cpu    = 5000
    memory = 5000
  }
}
`,
		"templates/reader.acl.hcl.tpl": `
policy "[[ var "team" . ]]-read" {
  description = "Read access to the team namespace"
  rules = <<EOT
namespace "[[ var "team" . ]]" { policy = "read" }
EOT
}
`,
		"templates/config.var.hcl.tpl": `
namespace = "[[ var "team" . ]]"
path      = "config/[[ var "team" . ]]"
items {
  user = "admin"
}
secret_items = ["password"]
`,
		"templates/web.nomad.tpl":    extendedPackJob("web"),
		"templates/worker.nomad.tpl": extendedPackJob("worker"),
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		must.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	return dir
}

func extendedPackJob(name string) string {
	return `
job "[[ var "team" . ]]-` + name + `" {
  namespace   = "[[ var "team" . ]]"
  datacenters = ["dc1"]
  group "app" {
    task "main" {
      driver = "mock_driver"
      config {
        run_for = "1h"
      }
      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
`
}

// TestCLI_ReleaseExtendedPack installs a pack with non-job resources in one
// step and removes everything on uninstall.
func TestCLI_ReleaseExtendedPack(t *testing.T) {
	isolateSupplyChain(t)
	packDir := writeExtendedPack(t)

	ct.HTTPTestWithACL(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		token := "--token=" + s.Config.Client.Meta["token"]
		client, err := ct.NewTestClient(s)
		must.NoError(t, err)

		r := runPackCmd(t, []string{"render", packDir})
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), "team.namespace.hcl")

		r = runTestPackCmd(t, s, []string{"run", packDir, "--detach", token, "--var-secret=config/blue:password=s3cret"})
		expectExit(t, r, 0)
		out := r.cmdOut.String()
		must.StrContains(t, out, `Release "team" deployed at revision 1`)

		ns, _, err := client.Namespaces().Info("blue", nil)
		must.NoError(t, err)
		must.Eq(t, "blue-quota", ns.Quota)
		_, _, err = client.Quotas().Info("blue-quota", nil)
		must.NoError(t, err)
		_, _, err = client.ACLPolicies().Info("blue-read", nil)
		must.NoError(t, err)
		v, _, err := client.Variables().Read("config/blue", &api.QueryOptions{Namespace: "blue"})
		must.NoError(t, err)
		must.Eq(t, "s3cret", v.Items["password"])
		for _, id := range []string{"blue-web", "blue-worker"} {
			_, _, err := client.Jobs().Info(id, &api.QueryOptions{Namespace: "blue"})
			must.NoError(t, err)
		}

		// The secret is not in the revision.
		rev, _, err := client.Releases().Revision("team", 1, nil)
		must.NoError(t, err)
		for _, res := range rev.Resources {
			if res.Type == api.ReleaseResourceVariable {
				must.MapNotContainsKey(t, res.Variable.Items, "password")
			}
		}

		// An upgrade without -var-secret keeps the stored secret.
		r = runTestPackCmd(t, s, []string{"upgrade", packDir, "--detach", token})
		expectExit(t, r, 0)
		v, _, err = client.Variables().Read("config/blue", &api.QueryOptions{Namespace: "blue"})
		must.NoError(t, err)
		must.Eq(t, "s3cret", v.Items["password"])

		r = runTestPackCmd(t, s, []string{"uninstall", "team", token})
		expectExit(t, r, 0)
		_, _, err = client.Namespaces().Info("blue", nil)
		must.ErrorContains(t, err, "404")
		_, _, err = client.Quotas().Info("blue-quota", nil)
		must.Error(t, err)
		_, _, err = client.ACLPolicies().Info("blue-read", nil)
		must.ErrorContains(t, err, "404")
		_, _, err = client.Jobs().Info("blue-web", &api.QueryOptions{Namespace: "blue"})
		must.Error(t, err)
	})
}

// TestCLI_ReleaseClientSide deploys from the client and records the
// revision on the servers (client-side apply).
func TestCLI_ReleaseClientSide(t *testing.T) {
	isolateSupplyChain(t)
	packDir := testfixture.Clone(t, communityRedis)

	ct.HTTPTest(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		client, err := ct.NewTestClient(s)
		must.NoError(t, err)

		r := runTestPackCmd(t, s, append([]string{"run", packDir, "--detach", "--client-side"}, redisVars...))
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), `Recorded revision 1 of release "redis"`)

		rel, _, err := client.Releases().Info("redis", nil)
		must.NoError(t, err)
		must.Len(t, 1, rel.Status.Resources)
		must.Eq(t, "redis", rel.Status.Resources[0].Name)

		// The recorded release can be rolled forward by the servers.
		r = runTestPackCmd(t, s, append([]string{"upgrade", packDir, "--detach"}, redisVars...))
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), "revision 2")
	})
}

// TestCLI_ReleaseResourcesNeedServer rejects packs with non-job resources
// on the client-side path.
func TestCLI_ReleaseResourcesNeedServer(t *testing.T) {
	isolateSupplyChain(t)
	packDir := writeExtendedPack(t)

	ct.HTTPTest(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		r := runTestPackCmd(t, s, []string{"run", packDir, "--detach", "--client-side"})
		expectExit(t, r, 1)
		must.StrContains(t, r.cmdOut.String(), "need servers with pack release support")
	})
}
