// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/hashicorp/nomad/jobspec2"
	"github.com/shoenig/test/must"

	ct "github.com/hashicorp/nomad/pack/internal/cli/testhelper"
)

// The tests in this file render the consul pack of the Norca pack registry
// (a copy in testdata/packs/consul) with several variable sets, parse and
// validate the rendered jobs, and plan them against a Norca agent. Nothing
// pulls or runs the Consul image: the test agent has no docker driver.

// repoPack copies testdata/packs/<name>, a copy of a pack of the Norca pack
// registry (see testdata/README.md), into a temporary directory (rendering
// may write a lock file) and returns the copy.
func repoPack(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	must.True(t, ok)
	src := filepath.Join(filepath.Dir(file), "testdata", "packs", name)
	dst := filepath.Join(t.TempDir(), name)
	must.NoError(t, os.CopyFS(dst, os.DirFS(src)))
	return dst
}

// renderJobs renders the pack into a directory and returns the rendered jobs
// by file name (e.g. "consul.nomad") and the output of the command.
func renderJobs(t *testing.T, packDir string, vars ...string) (map[string]string, string) {
	t.Helper()
	out := t.TempDir()
	args := append([]string{"render", packDir, "--to-dir=" + out, "--render-output-template"}, vars...)
	r := runPackCmd(t, args)
	expectExit(t, r, 0)

	jobs := map[string]string{}
	err := filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".nomad") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		jobs[filepath.Base(path)] = string(b)
		return nil
	})
	must.NoError(t, err)
	return jobs, r.cmdOut.String()
}

// parseConsulJob parses a rendered job like the agent's parse endpoint and
// validates it like the job register and validate endpoints.
func parseConsulJob(t *testing.T, name, src string) *api.Job {
	t.Helper()
	job, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{
		Path:   name,
		Body:   []byte(src),
		Strict: true,
	})
	must.NoError(t, err, must.Sprintf("%s:\n%s", name, src))
	job.Canonicalize()
	sj := agent.ApiJobToStructJob(job)
	sj.Canonicalize()
	must.NoError(t, sj.Validate(), must.Sprintf("%s:\n%s", name, src))
	return job
}

func taskByName(t *testing.T, tg *api.TaskGroup, name string) *api.Task {
	t.Helper()
	for _, task := range tg.Tasks {
		if task.Name == name {
			return task
		}
	}
	t.Fatalf("task %q not found in group %q", name, *tg.Name)
	return nil
}

func templateData(task *api.Task, dest string) string {
	for _, tmpl := range task.Templates {
		if tmpl.DestPath != nil && *tmpl.DestPath == dest {
			return *tmpl.EmbeddedTmpl
		}
	}
	return ""
}

const acceptConsulLicense = "--var=consul_license_accepted=true"

func TestConsulPack_RequiresLicenseAcceptance(t *testing.T) {
	isolateSupplyChain(t)
	packDir := repoPack(t, "consul")

	r := runPackCmd(t, []string{"render", packDir})
	expectExit(t, r, 1)
	must.StrContains(t, r.cmdOut.String()+r.cmdErr.String(), "Business Source License 1.1")
}

func TestConsulPack_RenderDefaults(t *testing.T) {
	isolateSupplyChain(t)
	packDir := repoPack(t, "consul")

	jobs, out := renderJobs(t, packDir, acceptConsulLicense)
	must.MapLen(t, 2, jobs) // no snapshot job by default
	must.StrContains(t, out, `norca acl policy apply -namespace default -job consul -group servers -task bootstrap`)
	must.StrContains(t, out, `path "nomad/jobs/consul/servers/bootstrap"`)
	must.StrContains(t, out, "BSL 1.1")

	servers := parseConsulJob(t, "consul.nomad", jobs["consul.nomad"])
	must.Eq(t, "consul", *servers.ID)
	must.Eq(t, "service", *servers.Type)
	tg := servers.TaskGroups[0]
	must.Eq(t, "servers", *tg.Name)
	must.Eq(t, 3, *tg.Count)
	must.Eq(t, "${meta.zone}", tg.Spreads[0].Attribute)
	must.Eq(t, "${meta.consul_server}", tg.Constraints[1].LTarget)
	must.Eq(t, "consul-server", tg.Volumes["data"].Source)
	must.Eq(t, 1, *tg.Update.MaxParallel)
	must.Eq(t, "1", *tg.Disruption.MaxUnavailable)

	boot := taskByName(t, tg, "bootstrap")
	must.Eq(t, "prestart", boot.Lifecycle.Hook)
	must.Eq(t, "exec", boot.Driver)
	must.True(t, boot.Identity.Env)
	must.Eq(t, "unix://${NOMAD_SECRETS_DIR}/api.sock", boot.Env["NORCA_ADDR"])
	script := templateData(boot, "local/bootstrap.sh")
	must.StrContains(t, script, `-check-index=0 "$src"`)
	must.StrContains(t, script, `-region="dc1" -domain="consul"`)
	must.StrContains(t, script, `"nomad/jobs/consul-clients"`)
	must.StrNotContains(t, script, "consul-snapshot")
	must.StrNotContains(t, script, "${") // no HCL interpolation in the script

	server := taskByName(t, tg, "server")
	must.Eq(t, "docker", server.Driver)
	must.Eq(t, "docker.io/hashicorp/consul:2.1.0", server.Config["image"])
	must.Eq(t, "host", server.Config["network_mode"])
	must.Eq(t, "/consul/data", *server.VolumeMounts[0].Destination)
	conf := templateData(server, "secrets/consul.d/consul.hcl")
	for _, want := range []string{
		`bootstrap_expect = 3`,
		`default_policy           = "deny"`,
		`initial_management = "{{ .management_token.Value | trimSpace }}"`,
		`http     = "127.0.0.1"`,
		`https    = "127.0.0.1 {{ $ip }}"`,
		`verify_server_hostname = true`,
		`allow_tls = true`,
		`encrypt_verify_incoming = true`,
		`data_dir         = "/consul/data"`,
	} {
		must.StrContains(t, conf, want)
	}
	must.StrContains(t, templateData(server, "secrets/consul.d/join.json"), `nomadService "consul-serf"`)

	acl := taskByName(t, tg, "acl-setup")
	must.Eq(t, "poststart", acl.Lifecycle.Hook)
	must.Eq[any](t, []any{"/bin/sh"}, acl.Config["entrypoint"])
	must.StrContains(t, templateData(acl, "local/acl-setup.sh"), "ensure_token \"$AGENT_ACCESSOR\"")

	clients := parseConsulJob(t, "consul-clients.nomad", jobs["consul-clients.nomad"])
	must.Eq(t, "consul-clients", *clients.ID)
	must.Eq(t, "system", *clients.Type)
	ctg := clients.TaskGroups[0]
	must.Eq(t, "!=", ctg.Constraints[0].Operand)
	client := taskByName(t, ctg, "client")
	cconf := templateData(client, "secrets/consul.d/consul.hcl")
	must.StrContains(t, cconf, `nomadVar "nomad/jobs/consul-clients"`)
	must.StrContains(t, cconf, `tls = true`)
	must.StrNotContains(t, cconf, "initial_management")
}

func TestConsulPack_RenderVariants(t *testing.T) {
	isolateSupplyChain(t)
	packDir := repoPack(t, "consul")

	jobs, out := renderJobs(t, packDir, acceptConsulLicense,
		"--var=job_name=sd",
		"--var=driver=exec",
		"--var=server_count=5",
		"--var=tls_enabled=false",
		"--var=zone_separate_required=true",
		"--var=host_network=wg",
		"--var=datacenter=eu-aws",
		"--var=download_base_url=https://mirror.internal/consul/",
		"--var=download_checksum=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"--var=snapshot_enabled=true",
		"--var=snapshot_s3_bucket=backups",
		"--var=snapshot_s3_endpoint=https://minio.internal:9000",
	)
	must.MapLen(t, 3, jobs)
	must.StrContains(t, out, "nomad/jobs/sd-snapshot aws_access_key_id=")

	servers := parseConsulJob(t, "consul.nomad", jobs["consul.nomad"])
	tg := servers.TaskGroups[0]
	must.Eq(t, 5, *tg.Count)
	must.Len(t, 1, tg.Separate)
	must.Eq(t, "wg", tg.Networks[0].ReservedPorts[0].HostNetwork)
	server := taskByName(t, tg, "server")
	must.Eq(t, "exec", server.Driver)
	must.Eq(t, "root", server.User)
	must.Eq(t, "local/consul", server.Config["command"])
	must.Eq(t, "https://mirror.internal/consul/2.1.0/consul_2.1.0_linux_amd64.zip", *server.Artifacts[0].GetterSource)
	must.Eq(t, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", server.Artifacts[0].GetterOptions["checksum"])
	conf := templateData(server, "secrets/consul.d/consul.hcl")
	must.StrContains(t, conf, `http     = "127.0.0.1 {{ $ip }}"`)
	must.StrContains(t, conf, `https    = -1`)
	must.StrNotContains(t, conf, "tls {")
	acl := taskByName(t, tg, "acl-setup")
	must.Eq(t, "${NOMAD_TASK_DIR}/consul", acl.Env["CONSUL_BIN"])
	must.StrContains(t, templateData(acl, "local/acl-setup.sh"), "norca-snapshot")
	must.StrContains(t, templateData(taskByName(t, tg, "bootstrap"), "local/bootstrap.sh"),
		`"nomad/jobs/sd-snapshot/snapshot"`)

	parseConsulJob(t, "consul-clients.nomad", jobs["consul-clients.nomad"])

	snap := parseConsulJob(t, "consul-snapshot.nomad", jobs["consul-snapshot.nomad"])
	must.Eq(t, "sd-snapshot", *snap.ID)
	must.Eq(t, "batch", *snap.Type)
	must.Eq(t, "0 * * * *", *snap.Periodic.Spec)
	stg := snap.TaskGroups[0]
	upload := taskByName(t, stg, "upload")
	must.Eq(t, "docker.io/amazon/aws-cli:2.27.0", upload.Config["image"])
	up := templateData(upload, "local/upload.sh")
	must.StrContains(t, up, `"s3://backups/$key"`)
	must.StrContains(t, up, `--endpoint-url "https://minio.internal:9000"`)
	must.StrContains(t, templateData(taskByName(t, stg, "save"), "local/save.sh"), "snapshot save")

	// Clients disabled, image from a mirror pinned by digest.
	jobs, _ = renderJobs(t, packDir, acceptConsulLicense,
		"--var=clients_enabled=false",
		"--var=image_registry=registry.internal:5000",
		"--var=image_digest=sha256:abcd",
	)
	must.MapLen(t, 1, jobs)
	servers = parseConsulJob(t, "consul.nomad", jobs["consul.nomad"])
	must.Eq(t, "registry.internal:5000/hashicorp/consul:2.1.0@sha256:abcd",
		taskByName(t, servers.TaskGroups[0], "server").Config["image"])
}

func TestConsulPack_RenderInvalid(t *testing.T) {
	isolateSupplyChain(t)
	packDir := repoPack(t, "consul")

	for _, tc := range []struct {
		vars []string
		err  string
	}{
		{[]string{"--var=server_count=4"}, "server_count must be 3 or 5"},
		{[]string{"--var=driver=raw_exec"}, "raw_exec cannot mount host volumes"},
		{[]string{"--var=driver=podman"}, "driver must be docker, exec or raw_exec"},
		{[]string{"--var=snapshot_enabled=true"}, "snapshot_enabled needs snapshot_s3_bucket"},
		{[]string{"--var=image_digest=abcd"}, "image_digest must be sha256 followed by"},
	} {
		r := runPackCmd(t, append([]string{"render", packDir, acceptConsulLicense}, tc.vars...))
		expectExit(t, r, 1)
		must.StrContains(t, r.cmdOut.String()+r.cmdErr.String(), tc.err)
	}
}

// TestConsulPack_Plan plans the pack against a Norca agent, which parses and
// validates the jobs server-side like "norca job validate". The agent has no
// docker driver and no Consul, so nothing is placed or pulled.
func TestConsulPack_Plan(t *testing.T) {
	isolateSupplyChain(t)
	packDir := repoPack(t, "consul")

	ct.HTTPTest(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		for _, vars := range [][]string{
			{acceptConsulLicense},
			{acceptConsulLicense, "--var=driver=exec", "--var=tls_enabled=false",
				"--var=snapshot_enabled=true", "--var=snapshot_s3_bucket=backups"},
		} {
			r := runTestPackCmd(t, s, append([]string{"plan", packDir}, vars...))
			must.StrContains(t, r.cmdOut.String(), "Plan succeeded", must.Sprintf("output: %s\n%s", r.cmdOut.String(), r.cmdErr.String()))
			must.Eq(t, 1, r.exitCode) // changes would be made
		}
	})
}
