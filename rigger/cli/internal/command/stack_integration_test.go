// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build integration

// The commands of phase 2 against a running agent (NORCA_ADDR, default
// http://127.0.0.1:4646; make integration).

package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
)

// integrationStack is a stack of two releases: a Variable with a generated
// token, and a job that reads the path of the Variable.
var integrationStack = map[string]string{
	"stack.hcl": `rig {
  name      = "rigit"
  region    = "global"
  namespace = "default"
}

module "config" {
  source = "./modules/config"
  values = { greeting = var.greeting, token = "!generate:config-token" }
}

module "worker" {
  source = "./modules/worker"
  values = { config_path = module.config.path }
  wait   = false
}

module "cache" {
  source = "builtin/redis"
  values = { job_name = "rigit-cache", auth_enabled = true, redis_password = "!generate:cache" }
  wait   = false
}
`,
	variablesFile:                   "variable \"greeting\" {\n  default = \"hello\"\n}\n",
	"secrets.hcl":                   "secret \"rigit/app\" {\n  items = { token = \"!generate:config-token\" }\n}\n",
	"modules/config/metadata.hcl":   "module {\n  name    = \"config\"\n  version = \"1.0.0\"\n}\n",
	"modules/config/variables.hcl":  "variable \"greeting\" {}\n\nvariable \"token\" {\n  sensitive = true\n}\n",
	"modules/config/config.var.hcl": "variable \"config\" {\n  path = \"rigit/${rig.release}\"\n\n  items {\n    greeting = var.greeting\n    token    = var.token\n  }\n}\n",
	"modules/config/outputs.hcl":    "output \"path\" {\n  value = \"rigit/${rig.release}\"\n}\n",
	"modules/worker/metadata.hcl":   "module {\n  name    = \"worker\"\n  version = \"1.0.0\"\n}\n",
	"modules/worker/variables.hcl":  "variable \"config_path\" {}\n",
	"modules/worker/worker.norca.hcl": `job "worker" {
  id          = "rigit-${rig.release}"
  datacenters = ["dc1"]

  group "w" {
    task "t" {
      driver = "docker"

      config {
        image = "busybox:1.36"
        args  = ["sleep", "3600"]
      }

      env {
        CONFIG = var.config_path
        ALLOC  = "${NORCA_ALLOC_ID}"
      }

      resources {
        cpu    = 20
        memory = 16
      }
    }
  }
}
`,
}

func TestIntegration_Stack(t *testing.T) {
	dir := writeTree(t, integrationStack)

	client, err := api.NewClient(api.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		for _, name := range []string{"config", "worker", "worker2", "cache", "solo"} {
			_, _, _ = client.Releases().Delete(name, &api.ReleaseDeleteOptions{Propagation: api.ReleasePropagationBackground}, nil)
		}
	})

	t.Setenv("RIG_CACHE_DIR", t.TempDir())
	rigRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)
	rigRun(t, exitChanges, []string{"+ config", "+ worker", "+ cache", "redis 0.3.0", "install", "secret rigit/app", "create", "generate cache, config-token"}, "stack", "plan", dir)
	rigRun(t, exitOK, []string{"install default/config: done", "install default/worker: done", "install default/cache: done"}, "stack", "apply", dir)
	rigRun(t, exitOK, []string{"= config", "= worker", "= cache", "nothing to do"}, "stack", "plan", dir)
	checkCluster(t, client)
	rigRun(t, exitChanges, []string{"~ config", "upgrade", "= worker"}, "stack", "plan", dir, "-var", "greeting=hi")
	rigRun(t, exitOK, []string{"config", "worker", "in sync"}, "stack", "status", dir)
	prune(t, dir)

	// One release without a stack.
	module := filepath.Join(dir, "modules", "config")
	rigRun(t, exitOK, []string{"install default/solo: revision 1"}, "install", module, "-name", "solo", "-var", "greeting=x", "-var", "token=y")
	rigRun(t, exitOK, []string{"default/solo is unchanged"}, "install", module, "-name", "solo", "-var", "greeting=x", "-var", "token=y")
	rigRun(t, exitOK, []string{"solo", "config 1.0.0", "-"}, "release", "list")
	rigRun(t, exitOK, []string{"solo"}, "release", "history", "solo")
	rigRun(t, exitOK, []string{"uninstalled default/solo"}, "release", "uninstall", "solo")
}

// checkCluster checks what the apply wrote: the generated token in the
// Variable and the secret, never in the revision; runtime references in
// the job as written.
func checkCluster(t *testing.T, client *api.Client) {
	t.Helper()

	secret, _, err := client.Secrets().Read("rigit/app", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	token := secret.Items["token"]

	v, _, err := client.Variables().Read("rigit/config", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(token) != 32 || v.Items["token"] != token || v.Items["greeting"] != "hello" {
		t.Errorf("token %q, Variable %v: want the generated token in both", token, v.Items)
	}

	rel, _, err := client.Releases().Info("config", nil)
	if err != nil {
		t.Fatal(err)
	}

	rev, _, err := client.Releases().Revision("config", rel.Spec.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}

	if data, _ := json.Marshal(rev); strings.Contains(string(data), token) || rev.Spec.Description != "rig stack rigit" {
		t.Errorf("the revision holds the token or no owner: %s", data)
	}

	job, _, err := client.Jobs().Info("rigit-worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	if env := job.TaskGroups[0].Tasks[0].Env; env["CONFIG"] != "rigit/config" || env["ALLOC"] != "${NORCA_ALLOC_ID}" {
		t.Errorf("env of the job = %v", env)
	}
}

// prune takes the worker out of the stack: shown, removed only with
// -prune; then destroys the stack.
func prune(t *testing.T, dir string) {
	t.Helper()

	stack := strings.Replace(integrationStack["stack.hcl"], "module \"worker\"", "module \"worker2\"", 1)
	if err := os.WriteFile(filepath.Join(dir, "stack.hcl"), []byte(stack), 0o600); err != nil {
		t.Fatal(err)
	}

	rigRun(t, exitChanges, []string{"- worker", "remove (with -prune)", "+ worker2"}, "stack", "plan", dir)
	rigRun(t, exitOK, []string{"remove default/worker: done"}, "stack", "apply", dir, "-prune")
	rigRun(t, exitOK, []string{"uninstalled default/"}, "stack", "destroy", dir)
}
