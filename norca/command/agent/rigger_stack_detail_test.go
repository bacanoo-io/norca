// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// siteStackFiles is a published stack of a registry repository: its
// metadata with a tab, a variable on the tab, a README.
var siteStackFiles = map[string]string{
	"stacks/site/metadata.hcl":  "stack {\n  name     = \"site\"\n  version  = \"1.0.0\"\n  category = \"platform\"\n}\n\nui {\n  tab \"general\" { label = \"General\" }\n}\n",
	"stacks/site/stack.hcl":     "rigger {\n  name = \"site\"\n}\n",
	"stacks/site/variables.hcl": "variable \"domain\" {\n  type = string\n  ui { tab = \"general\" }\n}\n",
	"stacks/site/README.md":     "# site\n",
}

// stackRegistryRepo is a registry repository with the stack site, tagged
// site/v1.0.0.
func stackRegistryRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	must.NoError(t, err)

	wt, err := r.Worktree()
	must.NoError(t, err)

	for p, data := range siteStackFiles {
		file := filepath.Join(dir, filepath.FromSlash(p))
		must.NoError(t, os.MkdirAll(filepath.Dir(file), 0o750))
		must.NoError(t, os.WriteFile(file, []byte(data), 0o600))
		_, err = wt.Add(p)
		must.NoError(t, err)
	}

	h, err := wt.Commit("site 1.0.0", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)}})
	must.NoError(t, err)

	_, err = r.CreateTag("site/v1.0.0", h, nil)
	must.NoError(t, err)

	return dir
}

// A published stack of a registry has a detail as a module has: the
// address to apply it by, its metadata, README and versions, and the form
// of its variables.
func TestHTTP_RiggerStackDetail(t *testing.T) {
	ci.Parallel(t)

	repo := stackRegistryRepo(t)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		obj := &api.CustomObject{Group: api.RiggerObjectGroup, Kind: api.RiggerRegistryKind, Name: "acme", Spec: map[string]any{"type": "git", "source": repo}}
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Objects().Apply(obj, nil, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))

		_, _, err := client.Rigger().SyncRegistry("acme", nil)
		must.NoError(t, err)

		detail, _, err := client.Rigger().StackDetail("acme", "site", "", nil)
		must.NoError(t, err)
		must.Eq(t, repo+"//stacks/site", detail.Source)
		must.Eq(t, "site", detail.Metadata["name"])
		must.Eq(t, "platform", detail.Metadata["category"])
		must.Eq(t, []string{"1.0.0"}, detail.Versions)
		must.Eq(t, "# site\n", detail.Readme)
		must.True(t, detail.Meets)

		props, _ := detail.FormSchema["properties"].(map[string]any)
		domain, _ := props["domain"].(map[string]any)
		must.Eq(t, "general", domain["x-ui-tab"])

		_, _, err = client.Rigger().StackDetail("acme", "missing", "", nil)
		must.ErrorContains(t, err, "404")
	})
}
