// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/shoenig/test/must"
)

func TestParse_SecretDelivery(t *testing.T) {
	ci.Parallel(t)

	hcl := `
job "web" {
  secrets {
    default_provider = "norca"
  }

  group "api" {
    secrets {
      default_provider = "vault"
    }

    secret_access {
      path = "jobs/${job}/*"
    }

    task "server" {
      driver = "docker"

      secret_access {
        path = "jobs/${job}/${group}/${task}"
      }

      secret "db" {
        provider = "norca"
        path     = "jobs/${job}/db"
        version  = 3

        file {
          destination = "secrets/db.env"
          format      = "env"
        }
        env {
          DB_USER     = "username"
          DB_PASSWORD = "password"
        }

        change_mode   = "signal"
        change_signal = "SIGHUP"
        timeout       = "30s"
      }

      secret "cert" {
        path     = "certs/web"
        optional = true
        file {
          destination = "secrets/cert.pem"
          format      = "template"
          template    = "{{ .cert }}"
        }
        change_mode = "script"
        change_script {
          command = "/bin/reload"
          args    = ["-a"]
        }
      }
    }
  }
}
`
	out, err := ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(hcl), AllowFS: true})
	must.NoError(t, err)

	must.Eq(t, &api.SecretsConfig{DefaultProvider: "norca"}, out.Secrets)
	tg := out.TaskGroups[0]
	must.Eq(t, &api.SecretsConfig{DefaultProvider: "vault"}, tg.Secrets)
	must.Eq(t, []*api.SecretAccess{{Path: "jobs/${job}/*"}}, tg.SecretAccess)
	task := tg.Tasks[0]
	must.Eq(t, []*api.SecretAccess{{Path: "jobs/${job}/${group}/${task}"}}, task.SecretAccess)
	must.Len(t, 2, task.Secrets)
	must.Eq(t, &api.TaskSecret{
		Name:         "db",
		Provider:     "norca",
		Path:         "jobs/${job}/db",
		Version:      3,
		File:         &api.TaskSecretFile{Destination: "secrets/db.env", Format: "env"},
		Env:          map[string]string{"DB_USER": "username", "DB_PASSWORD": "password"},
		ChangeMode:   "signal",
		ChangeSignal: "SIGHUP",
		Timeout:      30 * time.Second,
	}, task.Secrets[0])
	must.Eq(t, &api.TaskSecret{
		Name:       "cert",
		Path:       "certs/web",
		Optional:   true,
		File:       &api.TaskSecretFile{Destination: "secrets/cert.pem", Format: "template", Template: "{{ .cert }}"},
		ChangeMode: "script",
		ChangeScript: &api.ChangeScript{
			Command:     pointer.Of("/bin/reload"),
			Args:        []string{"-a"},
			Timeout:     pointer.Of(5 * time.Second),
			FailOnError: pointer.Of(false),
		},
	}, task.Secrets[1])
}
