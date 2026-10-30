// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"
)

// outputsTemplate is a text template like the outputs.tpl of a pack: HCL
// formatting would break it ("-namespace" -> "- namespace").
const outputsTemplate = `[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $ns := .namespace ]]
Apply the policy before the first run:
  norca acl policy apply -namespace [[ $ns ]] -job app \
    app-bootstrap app-bootstrap.hcl

namespace "[[ $ns ]]" {
  variables {
    path "nomad/jobs/app" { capabilities = ["read"] }
  }
}

The token (keep it safe):
  norca var get -namespace [[ $ns ]] -item=token nomad/jobs/app/<alloc id>
`

func TestFmtCommand_FormatTemplate_SkipsText(t *testing.T) {
	t.Parallel()
	c := &FmtCommand{}

	cases := map[string]string{
		"outputs":     outputsTemplate,
		"define":      "[[- define \"loc\" -]]\n  region = [[ .region | quote ]]\n[[- end -]]\n",
		"glued ident": "job \"x\" {\n  name = [[ .a ]]bar\n}\n",
		// parses as a subtraction, but formatting would add spaces around
		// the dash, which is part of the rendered name
		"operator after action": "job \"x\" {\n  meta {\n    name = [[ .a ]]-x\n  }\n}\n",
		// the brackets depend on the branch
		"branch brackets": `job "x" {
  group "g" {
    task "t" {
      config {
[[- if .docker ]]
        image = "busybox"
        args = [
[[- else ]]
        command = "/bin/app"
        args = [
[[- end ]]
          "run",
        ]
      }
    }
  }
}
`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.formatTemplate(input)
			var notHCL *notHCLTemplateError
			must.True(t, errors.As(err, &notHCL), must.Sprintf("err = %v", err))
		})
	}
}

func TestFmtCommand_FormatTemplate_ControlLines(t *testing.T) {
	t.Parallel()
	c := &FmtCommand{}

	input := `[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
job [[ $v.job_name | quote ]] {
[[- if ne $v.region "" ]]
region = [[ $v.region | quote ]]
[[- end ]]
  group "app" {
      count = [[ $v.count ]]
[[ template "constraints" . ]]
    task "web" {
      driver = "docker"
      template {
        data = <<EOF
[[- range $v.lines ]]
  line [[ . ]]
[[- end ]]
EOF
        destination = "local/[[ $v.job_name ]].conf"
      }
    }
  }
}
`
	want := `[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
job [[ $v.job_name | quote ]] {
  [[- if ne $v.region "" ]]
  region = [[ $v.region | quote ]]
  [[- end ]]
  group "app" {
    count = [[ $v.count ]]
    [[ template "constraints" . ]]
    task "web" {
      driver = "docker"
      template {
        data        = <<EOF
[[- range $v.lines ]]
  line [[ . ]]
[[- end ]]
EOF
        destination = "local/[[ $v.job_name ]].conf"
      }
    }
  }
}
`
	got, err := c.formatTemplate(input)
	must.NoError(t, err)
	must.Eq(t, want, got)

	// idempotent
	again, err := c.formatTemplate(got)
	must.NoError(t, err)
	must.Eq(t, want, again)
}

func TestCLI_Fmt_SkipsTextTemplates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"outputs.tpl":                  outputsTemplate,
		"templates/_helpers.tpl":       "[[- define \"loc\" -]]\n  region = [[ .region | quote ]]\n[[- end -]]\n",
		"templates/config.json.tpl":    "{\"a\":[[ .a ]]}\n",
		"templates/app.nomad.tpl":      "job \"app\" {\n  datacenters = [\"dc1\"]\n}\n",
		"templates/policy.acl.hcl.tpl": "namespace \"default\" {\n  policy = \"read\"\n}\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		must.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	result := runPackCmd(t, []string{"fmt", "-check", "-recursive", dir})
	must.Zero(t, result.exitCode)
	out := result.cmdOut.String()
	must.StrContains(t, out, "outputs.tpl")
	must.StrContains(t, out, "it defines template fragments")
	must.StrContains(t, out, "it renders a .json file")

	result = runPackCmd(t, []string{"fmt", "-recursive", dir})
	must.Zero(t, result.exitCode)
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		must.NoError(t, err)
		must.Eq(t, content, string(got), must.Sprint(name))
	}
}

func TestCLI_Fmt_InvalidHCL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "variables.hcl")
	content := "variable \"x\" {\n  default = -namespace <ns>\n"
	must.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	result := runPackCmdAllowErrors(t, []string{"fmt", dir})
	must.One(t, result.exitCode)
	got, err := os.ReadFile(path)
	must.NoError(t, err)
	must.Eq(t, content, string(got))
}
