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
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
)

// redisValuesSchema is a variables schema for the community redis pack.
const redisValuesSchema = `{
  "type": "object",
  "properties": {
    "app_count": {"type": "integer", "minimum": 1, "maximum": 3},
    "image": {"type": "string", "pattern": "^redis:"},
    "resources": {
      "type": "object",
      "properties": {
        "cpu": {"type": "number", "maximum": 4000},
        "memory": {"type": "number", "minimum": 64}
      }
    }
  }
}`

// cloneRedisWithSchema returns a copy of the community redis pack with a
// variables_schema in its metadata.
func cloneRedisWithSchema(t *testing.T) string {
	t.Helper()
	packDir := testfixture.Clone(t, communityRedis)
	must.NoError(t, os.WriteFile(filepath.Join(packDir, "values.schema.json"), []byte(redisValuesSchema), 0o644))
	md, err := os.ReadFile(filepath.Join(packDir, "metadata.hcl"))
	must.NoError(t, err)
	md = append(md, []byte("\nvariables_schema = file(\"values.schema.json\")\n")...)
	must.NoError(t, os.WriteFile(filepath.Join(packDir, "metadata.hcl"), md, 0o644))
	return packDir
}

// TestCLI_ReleaseVariablesSchema renders, plans and runs the redis pack with
// a variables schema: a variable file that violates the schema is rejected
// before anything is sent to the servers, with the paths of the variables.
func TestCLI_ReleaseVariablesSchema(t *testing.T) {
	isolateSupplyChain(t)
	packDir := cloneRedisWithSchema(t)

	badVars := filepath.Join(t.TempDir(), "bad.hcl")
	must.NoError(t, os.WriteFile(badVars, []byte("app_count = 5\nimage = \"nginx:1\"\nresources = {\n  cpu = 9000\n  memory = 256\n}\n"), 0o644))

	r := runPackCmd(t, append([]string{"render", packDir, "--var-file=" + badVars}, redisVars...))
	must.NonZero(t, r.exitCode)
	out := r.cmdOut.String()
	must.StrContains(t, out, `the variables of pack "redis" do not match its variables_schema`)
	must.StrContains(t, out, "at /app_count:")
	must.StrContains(t, out, "at /image:")
	must.StrContains(t, out, "at /resources/cpu:")

	// A --var override that violates the schema is rejected too.
	r = runPackCmd(t, append([]string{"render", packDir, "--var=app_count=0"}, redisVars...))
	must.NonZero(t, r.exitCode)
	must.StrContains(t, r.cmdOut.String(), "at /app_count:")

	// Valid values render.
	r = runPackCmd(t, append([]string{"render", packDir, "--var=app_count=2"}, redisVars...))
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), "count = 2")

	ct.HTTPTest(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		client, err := ct.NewTestClient(s)
		must.NoError(t, err)

		for _, cmd := range [][]string{{"plan"}, {"run", "--detach"}} {
			r := runTestPackCmd(t, s, append(append(cmd, packDir, "--var-file="+badVars), redisVars...))
			must.NonZero(t, r.exitCode)
			must.StrContains(t, r.cmdOut.String(), "at /app_count:")
		}
		_, _, err = client.Releases().Info("redis", nil)
		must.Error(t, err)

		r := runTestPackCmd(t, s, append([]string{"run", packDir, "--detach", "--var=app_count=2"}, redisVars...))
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), `Release "redis" deployed at revision 1`)

		// The schema is stored in the revision.
		rev, _, err := client.Releases().Revision("redis", 1, nil)
		must.NoError(t, err)
		must.Len(t, 1, rev.Spec.VariablesSchemas)
		must.Eq(t, "redis", rev.Spec.VariablesSchemas[0].Pack)
		must.Eq(t, redisValuesSchema, rev.Spec.VariablesSchemas[0].Schema)

		// The servers validate the values of API requests themselves.
		_, _, err = client.Releases().Plan("redis", &api.ReleaseApplyRequest{
			Resources: []*api.ReleaseResource{{Type: api.ReleaseResourceVariable, Variable: &api.Variable{
				Path: "config/redis", Items: api.VariableItems{"a": "b"},
			}}},
			VariablesSchemas: rev.Spec.VariablesSchemas,
			VariableValues:   map[string]map[string]any{"redis": {"app_count": 10}},
		}, nil)
		must.ErrorContains(t, err, "at /app_count:")

		r = runTestPackCmd(t, s, []string{"uninstall", "redis"})
		expectExit(t, r, 0)
	})
}

// authoritativeRegion sets the authoritative region of a test agent.
func authoritativeRegion(region string) ct.AgentOption {
	return func(c *agent.Config) {
		c.Server.AuthoritativeRegion = region
	}
}

// TestCLI_ReleaseMultiregionCommunityRedis installs the community redis pack
// in two regions in one operation, shows its state and history per region
// and uninstalls it in both.
func TestCLI_ReleaseMultiregionCommunityRedis(t *testing.T) {
	isolateSupplyChain(t)
	// The job of the pack has region = "global", which every region of the
	// install places in itself.
	packDir := testfixture.Clone(t, communityRedis)

	ct.HTTPTestMultiRegionCluster(t,
		ct.WithAgentConfig(ct.LogLevel("ERROR"), ct.Region("rA"), authoritativeRegion("rA")),
		ct.WithAgentConfig(ct.LogLevel("ERROR"), ct.Region("rB"), authoritativeRegion("rA")),
		func(s1, s2 *agent.TestAgent) {
			client, err := ct.NewTestClient(s1)
			must.NoError(t, err)

			// Plan from rB: both regions, the authoritative region first.
			r := runTestPackCmd(t, s2, append([]string{"plan", packDir, "--regions=rB,rA"}, redisVars...))
			must.Eq(t, 1, r.exitCode, must.Sprintf("output: %s", r.cmdOut.String()))
			must.StrContains(t, r.cmdOut.String(), `Region "rA":`)
			must.StrContains(t, r.cmdOut.String(), `Region "rB":`)
			must.StrContains(t, r.cmdOut.String(), "planned")

			r = runTestPackCmd(t, s2, append([]string{"run", packDir, "--detach", "--regions=rB,rA"}, redisVars...))
			expectExit(t, r, 0)
			must.StrContains(t, r.cmdOut.String(), `Release "redis" deployed in 2 regions`)

			for _, region := range []string{"rA", "rB"} {
				j, _, err := client.Jobs().Info("redis", &api.QueryOptions{Region: region})
				must.NoError(t, err, must.Sprint(region))
				must.Eq(t, region, *j.Region)
				must.Eq(t, "redis", j.Meta[api.ReleaseMetaRelease])

				rel, _, err := client.Releases().Info("redis", &api.QueryOptions{Region: region})
				must.NoError(t, err)
				must.Eq(t, api.ReleasePhaseDeployed, rel.Status.Phase)
				must.Eq(t, []string{"rA", "rB"}, rel.Spec.Multiregion.Regions)
				must.Eq(t, "rA", rel.Spec.Multiregion.GlobalRegion)
			}

			// Status and history show every region, from either region.
			r = runTestPackCmd(t, s1, []string{"status", "redis"})
			expectExit(t, r, 0)
			out := r.cmdOut.String()
			must.StrContains(t, out, "Regions:")
			must.StrContains(t, out, "rA (global)")
			must.StrContains(t, out, "rB")
			must.StrContains(t, out, "deployed")

			r = runTestPackCmd(t, s2, []string{"history", "redis"})
			expectExit(t, r, 0)
			must.StrContains(t, r.cmdOut.String(), `Region "rA" (global):`)
			must.StrContains(t, r.cmdOut.String(), `Region "rB":`)
			must.StrContains(t, r.cmdOut.String(), "install")

			// An upgrade of both regions creates revision 2 in each.
			r = runTestPackCmd(t, s1, append([]string{"upgrade", packDir, "--detach", "--regions=rA,rB", "--var=image=redis:7"}, redisVars...))
			expectExit(t, r, 0)
			for _, region := range []string{"rA", "rB"} {
				rel, _, err := client.Releases().Info("redis", &api.QueryOptions{Region: region})
				must.NoError(t, err)
				must.Eq(t, uint64(2), rel.Status.Revision)
				j, _, err := client.Jobs().Info("redis", &api.QueryOptions{Region: region})
				must.NoError(t, err)
				must.Eq(t, "redis:7", j.TaskGroups[0].Tasks[0].Config["image"])
			}

			// Uninstall in all regions of the install, the global one last.
			r = runTestPackCmd(t, s2, []string{"uninstall", "redis", "--all-regions"})
			expectExit(t, r, 0)
			must.StrContains(t, r.cmdOut.String(), `Release "redis" uninstalled in region "rB"`)
			must.StrContains(t, r.cmdOut.String(), `Release "redis" uninstalled in region "rA"`)
			for _, region := range []string{"rA", "rB"} {
				_, _, err := client.Releases().Info("redis", &api.QueryOptions{Region: region})
				must.Error(t, err)
				_, _, err = client.Jobs().Info("redis", &api.QueryOptions{Region: region})
				must.Error(t, err)
			}
		})
}
