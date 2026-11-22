// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

// testBundle is a bundle of the module web at a commit, with the digest its
// manifest says: the right one, or digest when not empty.
func testBundle(t *testing.T, digest string) []byte {
	t.Helper()

	root := fstest.MapFS{
		"modules/web/metadata.hcl":  {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")},
		"modules/web/variables.hcl": {Data: []byte("variable \"count\" {\n  default = 1\n}\n")},
	}

	if digest == "" {
		eng, err := rigger.New()
		must.NoError(t, err)

		digest, err = eng.ModuleDigest(t.Context(), root, "modules/web")
		must.NoError(t, err)
	}

	mod := rigger.SourcedModule{
		Source: "git.example.com/acme/modules//modules/web", Version: "1.0.0", Repository: "git.example.com/acme/modules",
		Path: "modules/web", Ref: "web/v1.0.0", Commit: strings.Repeat("a", 40), Digest: digest,
		Dirs: []string{"modules/web"}, Root: root,
	}

	var buf bytes.Buffer
	must.NoError(t, bundle.Write(&buf, "shop", []rigger.SourcedModule{mod}))

	return buf.Bytes()
}

// Bundles of the cluster over the HTTP API (design/rig-integration.md
// 4.6c): a bundle is checked and kept on the bus, listed and deleted; one
// whose content is not its digest, and something that is not a bundle, are
// refused; a token that can read registries lists bundles and imports none.
func TestHTTP_RiggerBundles(t *testing.T) {
	ci.Parallel(t)

	withBus := func(c *Config) { c.Server.Bus = &BusConfig{Enabled: pointer.Of(true)} }

	httpACLTest(t, withBus, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		var b *api.RiggerBundle

		// The bus of a new agent is ready a little after its leader.
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			var err error
			b, _, err = client.Rigger().ImportBundle(testBundle(t, ""), nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)))

		must.StrHasPrefix(t, "bundle-", b.Name)
		must.Eq(t, "shop", b.Stack)
		must.Eq(t, []string{"git.example.com/acme/modules//modules/web 1.0.0"}, b.Modules)

		list, _, err := client.Rigger().Bundles(nil)
		must.NoError(t, err)
		must.Len(t, 1, list)
		must.Eq(t, b.Name, list[0].Name)
		must.Eq(t, "shop", list[0].Stack)

		// The same bundle again: the same name, still one.
		again, _, err := client.Rigger().ImportBundle(testBundle(t, ""), nil)
		must.NoError(t, err)
		must.Eq(t, b.Name, again.Name)

		_, _, err = client.Rigger().ImportBundle(testBundle(t, "sha256:"+strings.Repeat("0", 64)), nil)
		must.ErrorContains(t, err, "the bundle holds content of the digest")

		_, _, err = client.Rigger().ImportBundle([]byte("not a bundle"), nil)
		must.ErrorContains(t, err, "not a bundle")

		reader := mock.CreatePolicyAndToken(t, s.Agent.Server().State(), 1800, "registry-read",
			`namespace "*" { object "rigger.norca.io/Registry" { capabilities = ["read"] } }`)
		client.SetSecretID(reader.SecretID)

		_, _, err = client.Rigger().Bundles(nil)
		must.NoError(t, err)

		_, _, err = client.Rigger().ImportBundle(testBundle(t, ""), nil)
		must.ErrorContains(t, err, "Permission denied")

		client.SetSecretID(s.RootToken.SecretID)

		_, err = client.Rigger().DeleteBundle(b.Name, nil)
		must.NoError(t, err)

		list, _, err = client.Rigger().Bundles(nil)
		must.NoError(t, err)
		must.SliceEmpty(t, list)

		_, err = client.Rigger().DeleteBundle(b.Name, nil)
		must.ErrorContains(t, err, "not found")
	})
}

// gitIn runs git in dir.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	must.NoError(t, err, must.Sprint(string(out)))
}

// A cluster without a network (design/rig-integration.md 4.6c): a stack
// whose module comes from a git repository the servers can't reach fails;
// with the bundle of its modules imported into the cluster, the servers
// plan and apply it from the bundle.
func TestHTTP_RiggerBundles_Offline(t *testing.T) {
	ci.Parallel(t)

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}

	repo := t.TempDir()
	for name, data := range map[string]string{
		"modules/greeter/metadata.hcl":  "module {\n  name    = \"greeter\"\n  version = \"1.0.0\"\n}\n",
		"modules/greeter/variables.hcl": "variable \"text\" {\n  default = \"hello\"\n}\n",
		"modules/greeter/g.var.hcl":     "variable \"g\" {\n  path = \"greeter/${rigger.release}\"\n\n  items {\n    text = var.text\n  }\n}\n",
	} {
		p := filepath.Join(repo, filepath.FromSlash(name))
		must.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must.NoError(t, os.WriteFile(p, []byte(data), 0o644))
	}

	gitIn(t, repo, "init", "--quiet", "-b", "main")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "greeter 1.0.0")
	gitIn(t, repo, "tag", "greeter/v1.0.0")

	stackDir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(stackDir, "stack.hcl"), []byte(`rigger {
  name      = "offline"
  region    = "global"
  namespace = "default"
}

module "hello" {
  source  = "file://`+filepath.ToSlash(repo)+`//modules/greeter"
  version = "~> 1.0"
}
`), 0o644))

	// Synced and bundled with the repository at hand.
	eng, err := rigger.New(rigger.WithFetcher(fetch.New(t.TempDir())))
	must.NoError(t, err)

	_, lock, diags := eng.SyncStack(t.Context(), os.DirFS(stackDir), ".", false)
	must.False(t, diags.HasErrors(), must.Sprint(diags))
	must.NoError(t, os.WriteFile(filepath.Join(stackDir, "rigger.lock.hcl"), lock, 0o644))

	st, diags := eng.LoadStack(t.Context(), os.DirFS(stackDir), ".")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	var bundled bytes.Buffer
	must.NoError(t, bundle.Write(&bundled, st.Name(), st.SourcedModules()))

	files, err := st.Archive()
	must.NoError(t, err)

	// The repository is gone.
	must.NoError(t, os.Rename(repo, repo+".gone"))
	t.Cleanup(func() { _ = os.RemoveAll(repo + ".gone") })

	withBus := func(c *Config) { c.Server.Bus = &BusConfig{Enabled: pointer.Of(true)} }

	httpACLTest(t, withBus, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		req := &api.RiggerStackRequest{Files: files}

		_, _, err := client.Rigger().StackPlan(req, nil)
		must.Error(t, err, must.Sprint("a module of a repository the servers can't reach, without a bundle"))

		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Rigger().ImportBundle(bundled.Bytes(), nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)))

		plan, _, err := client.Rigger().StackPlan(req, nil)
		must.NoError(t, err)
		must.Len(t, 1, plan.Plan.Releases)
		must.Eq(t, "install", plan.Plan.Releases[0].Action)

		op, _, err := client.Rigger().StackApply(req, nil)
		must.NoError(t, err)

		op = waitOperation(t, client, op.Name)
		must.Eq(t, "Succeeded", op.Status.Phase, must.Sprint(op.Status.Error))
	})
}
