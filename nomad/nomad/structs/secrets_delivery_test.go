// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func testTaskSecret() *TaskSecret {
	return &TaskSecret{
		Name: "db",
		Path: "jobs/${job}/db",
		File: &TaskSecretFile{Destination: "secrets/db.env"},
		Env:  map[string]string{"DB_PASSWORD": "password"},
	}
}

func TestTaskSecret_CanonicalizeValidate(t *testing.T) {
	ci.Parallel(t)

	s := testTaskSecret()
	s.Canonicalize()
	must.Eq(t, SecretChangeModeRestart, s.ChangeMode)
	must.Eq(t, DefaultSecretTimeout, s.Timeout)
	must.Eq(t, SecretFileFormatEnv, s.File.Format)
	must.NoError(t, s.Validate())

	cases := []struct {
		name   string
		modify func(*TaskSecret)
		err    string
	}{
		{"name", func(s *TaskSecret) { s.Name = "a b" }, "invalid secret name"},
		{"no path", func(s *TaskSecret) { s.Path = "" }, "secret requires a path"},
		{"unknown placeholder", func(s *TaskSecret) { s.Path = "jobs/${NOMAD_JOB_ID}/db" }, "only the ${job}"},
		{"norca path", func(s *TaskSecret) { s.Provider = "norca"; s.Path = "kv/../db" }, "invalid secret path"},
		{"provider", func(s *TaskSecret) { s.Provider = "a/b" }, "invalid provider name"},
		{"engine", func(s *TaskSecret) { s.Engine = "pg" }, "not supported yet"},
		{"no delivery", func(s *TaskSecret) { s.File = nil; s.Env = nil }, "file or env block"},
		{"outside secrets", func(s *TaskSecret) { s.File.Destination = "local/db.env" }, "must be below secrets/"},
		{"escape", func(s *TaskSecret) { s.File.Destination = "secrets/../local/x" }, "clean relative path"},
		{"absolute", func(s *TaskSecret) { s.File.Destination = "/secrets/x" }, "clean relative path"},
		{"dir itself", func(s *TaskSecret) { s.File.Destination = "secrets/" }, "clean relative path"},
		{"reserved", func(s *TaskSecret) { s.File.Destination = "secrets/nomad_token" }, "is reserved"},
		{"format", func(s *TaskSecret) { s.File.Format = "yaml" }, "invalid secret file format"},
		{"raw item", func(s *TaskSecret) { s.File.Format = "raw:" }, "needs an item"},
		{"template", func(s *TaskSecret) { s.File.Format = "template" }, "requires a template"},
		{"template format", func(s *TaskSecret) { s.File.Template = "{{ .a }}" }, "requires format template"},
		{"env name", func(s *TaskSecret) { s.Env = map[string]string{"1X": "a"} }, "invalid environment variable"},
		{"env prefix", func(s *TaskSecret) { s.Env = map[string]string{"NOMAD_X": "a"} }, "reserved"},
		{"env item", func(s *TaskSecret) { s.Env = map[string]string{"X": ""} }, "needs an item"},
		{"change mode", func(s *TaskSecret) { s.ChangeMode = "reload" }, "invalid change_mode"},
		{"signal", func(s *TaskSecret) { s.ChangeMode = SecretChangeModeSignal }, "requires a change_signal"},
		{"signal mode", func(s *TaskSecret) { s.ChangeSignal = "SIGHUP" }, "requires change_mode signal"},
		{"script", func(s *TaskSecret) { s.ChangeMode = SecretChangeModeScript }, "requires a change_script"},
		{"timeout", func(s *TaskSecret) { s.Timeout = -time.Second }, "timeout must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testTaskSecret()
			s.Canonicalize()
			tc.modify(s)
			err := s.Validate()
			must.Error(t, err)
			must.StrContains(t, err.Error(), tc.err)
		})
	}

	for _, f := range []string{"json", "raw:password", "env"} {
		s := testTaskSecret()
		s.File.Format = f
		s.Canonicalize()
		must.NoError(t, s.Validate(), must.Sprint(f))
	}
	s = testTaskSecret()
	s.File.Format, s.File.Template = "template", "{{ .password }}"
	s.ChangeMode, s.ChangeScript = SecretChangeModeScript, &ChangeScript{Command: "/bin/reload"}
	s.Canonicalize()
	must.NoError(t, s.Validate())
}

func TestTaskSecrets_TaskValidation(t *testing.T) {
	ci.Parallel(t)

	task := &Task{Name: "web"}
	a, b := testTaskSecret(), testTaskSecret()
	b.Name = "db2"
	task.Secrets = []*TaskSecret{a, b}
	for _, s := range task.Secrets {
		s.Canonicalize()
	}
	err := validateTaskSecrets(task)
	must.Error(t, err)
	must.StrContains(t, err.Error(), "the same destination")
	must.StrContains(t, err.Error(), "the same environment variable")

	b.Name = "db"
	b.File.Destination = "secrets/b"
	b.Env = nil
	err = validateTaskSecrets(task)
	must.StrContains(t, err.Error(), `duplicate secret "db"`)

	b.Name = "db2"
	task.Templates = []*Template{{DestPath: "secrets/b"}}
	must.StrContains(t, validateTaskSecrets(task).Error(), "destination of a template")
	task.Templates = nil
	must.NoError(t, validateTaskSecrets(task))

	task.SecretAccess = []*SecretAccess{{Path: "jobs/${job}/${task}/*"}}
	must.NoError(t, validateTaskSecrets(task))
	task.SecretAccess = []*SecretAccess{{Path: "jobs/${foo}"}}
	must.Error(t, validateTaskSecrets(task))

	must.NoError(t, validateSecretAccess([]*SecretAccess{{Path: "jobs/${job}/${group}/*"}}, false))
	must.Error(t, validateSecretAccess([]*SecretAccess{{Path: "jobs/${task}"}}, false))
	must.Error(t, validateSecretAccess([]*SecretAccess{{Path: "/jobs"}}, false))

	must.NoError(t, (&SecretsConfig{DefaultProvider: "bao"}).Validate())
	must.Error(t, (&SecretsConfig{DefaultProvider: "b a o"}).Validate())
}

func TestTaskSecret_CopyEqual(t *testing.T) {
	ci.Parallel(t)

	s := testTaskSecret()
	s.ChangeScript = &ChangeScript{Command: "x", Args: []string{"a"}}
	c := s.Copy()
	must.True(t, s.Equal(c))
	c.Env["X"] = "y"
	must.False(t, s.Equal(c))
	c = s.Copy()
	c.File.Format = "json"
	must.False(t, s.Equal(c))
	c = s.Copy()
	c.ChangeScript.Args[0] = "b"
	must.False(t, s.Equal(c))
	must.Eq(t, "a", s.ChangeScript.Args[0])

	task := &Task{Secrets: []*TaskSecret{s}, SecretAccess: []*SecretAccess{{Path: "a/*"}}}
	tc := task.Copy()
	must.True(t, TaskSecretsEqual(task.Secrets, tc.Secrets))
	tc.SecretAccess[0].Path = "b"
	must.Eq(t, "a/*", task.SecretAccess[0].Path)

	tg := &TaskGroup{Secrets: &SecretsConfig{DefaultProvider: "x"}, SecretAccess: []*SecretAccess{{Path: "a"}}}
	tgc := tg.Copy()
	tgc.Secrets.DefaultProvider = "y"
	tgc.SecretAccess[0].Path = "b"
	must.Eq(t, "x", tg.Secrets.DefaultProvider)
	must.Eq(t, "a", tg.SecretAccess[0].Path)

	j := &Job{Secrets: &SecretsConfig{DefaultProvider: "x"}}
	jc := j.Copy()
	jc.Secrets.DefaultProvider = "y"
	must.Eq(t, "x", j.Secrets.DefaultProvider)
}

func TestSecretAccess_PatternsAndGrants(t *testing.T) {
	ci.Parallel(t)

	job := &Job{ID: "web", TaskGroups: []*TaskGroup{{
		Name:         "api",
		SecretAccess: []*SecretAccess{{Path: "jobs/${job}/*"}},
		Tasks: []*Task{
			{Name: "server", SecretAccess: []*SecretAccess{{Path: "jobs/${job}/${group}/${task}"}}},
			{Name: "sidecar"},
		},
	}}}
	must.Eq(t, []string{"jobs/web/*", "jobs/web/api/server"}, SecretAccessPatterns(job, "api", "server"))
	must.Eq(t, []string{"jobs/web/*"}, SecretAccessPatterns(job, "api", "sidecar"))
	must.Nil(t, SecretAccessPatterns(job, "other", "x"))

	grants := job.SecretAccessGrants()
	must.Len(t, 2, grants)
	must.Eq(t, SecretAccessGrant{Group: "api", Pattern: "jobs/web/*"}, grants[0])
	must.Eq(t, SecretAccessGrant{Group: "api", Task: "server", Pattern: "jobs/web/api/server"}, grants[1])

	// Children of dispatched and periodic jobs use the parent's ID.
	child := job.Copy()
	child.ID, child.ParentID = "web/dispatch-123", "web"
	must.Eq(t, []string{"jobs/web/*"}, SecretAccessPatterns(child, "api", "sidecar"))
	must.True(t, job.UsesSecretDelivery())
	must.False(t, (&Job{TaskGroups: []*TaskGroup{{Tasks: []*Task{{}}}}}).UsesSecretDelivery())
}

func TestJob_SecretRefsAndDefaults(t *testing.T) {
	ci.Parallel(t)

	s := testTaskSecret()
	s.Canonicalize()
	job := &Job{
		ID:      "web",
		Secrets: &SecretsConfig{NamespaceDefaultProvider: "bao"},
		TaskGroups: []*TaskGroup{{
			Name: "api",
			Tasks: []*Task{{
				Name:    "server",
				Secrets: []*TaskSecret{s},
				Templates: []*Template{{EmbeddedTmpl: `{{ with nomadSecret "aws:prod/key" }}{{ .key }}{{ end }}
{{ nomadSecret "jobs/${job}/x" }}`}},
			}},
		}},
	}
	refs := job.SecretRefs()
	must.Len(t, 3, refs)
	must.Eq(t, &SecretRef{Group: "api", Task: "server", Source: "secret", Name: "db", DefaultProvider: "bao",
		Path: "jobs/web/db", FileFormat: "env", Env: []string{"DB_PASSWORD"}}, refs[0])
	must.Eq(t, "aws", refs[1].Provider)
	must.Eq(t, "prod/key", refs[1].Path)
	must.Eq(t, "", refs[2].Provider)
	must.Eq(t, "jobs/web/x", refs[2].Path)

	must.Eq(t, "bao", job.SecretDefaultProvider(job.TaskGroups[0]))
	job.Secrets.DefaultProvider = "vault"
	must.Eq(t, "vault", job.SecretDefaultProvider(job.TaskGroups[0]))
	job.TaskGroups[0].Secrets = &SecretsConfig{DefaultProvider: "norca"}
	must.Eq(t, "norca", job.SecretDefaultProvider(job.TaskGroups[0]))

	p, path := ParseSecretRef("bao:kv/data/db")
	must.Eq(t, "bao", p)
	must.Eq(t, "kv/data/db", path)
	p, path = ParseSecretRef("app/db")
	must.Eq(t, "", p)
	must.Eq(t, "app/db", path)
}

func TestJobDiff_Secrets(t *testing.T) {
	ci.Parallel(t)

	newJob := func() *Job {
		s := testTaskSecret()
		s.Canonicalize()
		return &Job{
			ID:      "web",
			Secrets: &SecretsConfig{DefaultProvider: "norca"},
			TaskGroups: []*TaskGroup{{
				Name:         "api",
				SecretAccess: []*SecretAccess{{Path: "jobs/${job}/*"}},
				Tasks:        []*Task{{Name: "server", Secrets: []*TaskSecret{s}}},
			}},
		}
	}
	old, next := newJob(), newJob()
	d, err := old.Diff(next, false)
	must.NoError(t, err)
	must.Eq(t, DiffTypeNone, d.Type)

	next.Secrets.DefaultProvider = "bao"
	next.TaskGroups[0].SecretAccess[0].Path = "jobs/other/*"
	sec := next.TaskGroups[0].Tasks[0].Secrets[0]
	sec.Path = "jobs/${job}/db2"
	sec.Env["DB_USER"] = "username"
	sec.File.Format = "json"
	next.TaskGroups[0].Tasks[0].Secrets = append(next.TaskGroups[0].Tasks[0].Secrets,
		&TaskSecret{Name: "cert", Path: "certs/web", File: &TaskSecretFile{Destination: "secrets/cert.pem", Format: "raw:cert"}})

	d, err = old.Diff(next, false)
	must.NoError(t, err)
	must.Eq(t, DiffTypeEdited, d.Type)
	out := d.GoString()
	for _, want := range []string{`"DefaultProvider"`, `"bao"`, "SecretAccess", "jobs/other/*",
		`"Path"`, "db2", "Env[DB_USER]", `"Format"`, `"json"`, "cert.pem"} {
		must.StrContains(t, out, want)
	}
	must.False(t, strings.Contains(out, "password\" => \"password"))

	// The default provider and secret blocks need new allocations.
	must.False(t, TaskSecretsEqual(old.TaskGroups[0].Tasks[0].Secrets, next.TaskGroups[0].Tasks[0].Secrets))
}
