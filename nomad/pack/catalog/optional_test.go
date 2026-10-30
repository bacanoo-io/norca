// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

// Release values reach the renderer as variable overrides. A list of objects
// with optional() attributes must accept items that leave them out, and get
// their defaults, as in a variables file (the users of the nats pack).
func TestRenderPack_OptionalObjectAttributes(t *testing.T) {
	ci.Parallel(t)
	dir := filepath.Join(t.TempDir(), "opt")
	files := map[string]string{
		"metadata.hcl": "app {\n  url = \"https://example.com\"\n}\n\npack {\n  name    = \"opt\"\n  version = \"0.1.0\"\n}\n",
		"variables.hcl": `variable "users" {
  type = list(object({
    name          = string
    account       = optional(string, "APP")
    publish_allow = optional(list(string), [])
  }))
  default = []
}
`,
		"templates/opt.nomad.tpl": `job "opt" {
  meta {
[[- range $i, $u := var "users" . ]]
    user_[[ $i ]] = "[[ $u.name ]]:[[ $u.account ]]:[[ len $u.publish_allow ]]"
[[- end ]]
  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "echo"
      }
    }
  }
}
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		must.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	out, err := RenderPack(RenderInput{
		PackDir:      dir,
		RegistryName: "test",
		PackName:     "opt",
		ReleaseName:  "opt",
		Values: map[string]any{"users": []any{
			map[string]any{"name": "app", "publish_allow": []any{"app.>"}},
			map[string]any{"name": "ops", "account": "SYS"},
		}},
	})
	must.NoError(t, err)

	var meta map[string]string
	for _, r := range out.Resources {
		if r.Job != nil {
			meta = r.Job.Meta
		}
	}
	must.Eq(t, "app:APP:1", meta["user_0"], must.Sprint("default of the optional account"))
	must.Eq(t, "ops:SYS:0", meta["user_1"], must.Sprint("default of the optional list"))
	must.False(t, strings.Contains(meta["user_1"], "<nil>"))
}
