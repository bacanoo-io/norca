// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestJobEndpoint_SecretDeliveryConversion(t *testing.T) {
	ci.Parallel(t)

	job := &api.Job{
		ID:      pointer.Of("web"),
		Name:    pointer.Of("web"),
		Secrets: &api.SecretsConfig{DefaultProvider: "norca", NamespaceDefaultProvider: "spoofed"},
		TaskGroups: []*api.TaskGroup{{
			Name:         pointer.Of("api"),
			Secrets:      &api.SecretsConfig{DefaultProvider: "vault"},
			SecretAccess: []*api.SecretAccess{{Path: "jobs/${job}/*"}},
			Tasks: []*api.Task{{
				Name:         "server",
				Driver:       "docker",
				SecretAccess: []*api.SecretAccess{{Path: "jobs/${job}/${task}"}},
				Secrets: []*api.TaskSecret{{
					Name:         "db",
					Provider:     "norca",
					Path:         "jobs/${job}/db",
					Version:      2,
					File:         &api.TaskSecretFile{Destination: "secrets/db.json", Format: "json"},
					Env:          map[string]string{"DB_PASSWORD": "password"},
					ChangeMode:   "script",
					ChangeScript: &api.ChangeScript{Command: pointer.Of("/bin/reload"), Args: []string{"x"}, Timeout: pointer.Of(time.Second), FailOnError: pointer.Of(true)},
					Optional:     true,
					Timeout:      time.Minute,
				}},
			}},
		}},
	}
	job.Canonicalize()

	// The API form survives a JSON round trip.
	data, err := json.Marshal(job)
	must.NoError(t, err)
	var decoded api.Job
	must.NoError(t, json.Unmarshal(data, &decoded))
	must.Eq(t, job.TaskGroups[0].Tasks[0].Secrets, decoded.TaskGroups[0].Tasks[0].Secrets)
	must.Eq(t, job.TaskGroups[0].SecretAccess, decoded.TaskGroups[0].SecretAccess)
	must.Eq(t, job.Secrets, decoded.Secrets)

	sj := ApiJobToStructJob(&decoded)
	must.Eq(t, &structs.SecretsConfig{DefaultProvider: "norca"}, sj.Secrets)
	tg := sj.TaskGroups[0]
	must.Eq(t, &structs.SecretsConfig{DefaultProvider: "vault"}, tg.Secrets)
	must.Eq(t, []*structs.SecretAccess{{Path: "jobs/${job}/*"}}, tg.SecretAccess)
	task := tg.Tasks[0]
	must.Eq(t, []*structs.SecretAccess{{Path: "jobs/${job}/${task}"}}, task.SecretAccess)
	must.Eq(t, []*structs.TaskSecret{{
		Name:         "db",
		Provider:     "norca",
		Path:         "jobs/${job}/db",
		Version:      2,
		File:         &structs.TaskSecretFile{Destination: "secrets/db.json", Format: "json"},
		Env:          map[string]string{"DB_PASSWORD": "password"},
		ChangeMode:   "script",
		ChangeScript: &structs.ChangeScript{Command: "/bin/reload", Args: []string{"x"}, Timeout: time.Second, FailOnError: true},
		Optional:     true,
		Timeout:      time.Minute,
	}}, task.Secrets)
}
