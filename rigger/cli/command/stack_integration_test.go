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
	stackFile: `rigger {
  name      = "riggerit"
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
  values = { job_name = "riggerit-cache", auth_enabled = true, redis_password = "!generate:cache" }
  wait   = false
}
`,
	variablesFile:                   "variable \"greeting\" {\n  default = \"hello\"\n}\n",
	"secrets.hcl":                   "secret \"riggerit/app\" {\n  items = { token = \"!generate:config-token\" }\n}\n",
	"modules/config/metadata.hcl":   "module {\n  name    = \"config\"\n  version = \"1.0.0\"\n}\n",
	"modules/config/variables.hcl":  "variable \"greeting\" {}\n\nvariable \"token\" {\n  sensitive = true\n}\n",
	"modules/config/config.var.hcl": "variable \"config\" {\n  path = \"riggerit/${rigger.release}\"\n\n  items {\n    greeting = var.greeting\n    token    = var.token\n  }\n}\n",
	"modules/config/outputs.hcl":    "output \"path\" {\n  value = \"riggerit/${rigger.release}\"\n}\n",
	"modules/worker/metadata.hcl":   "module {\n  name    = \"worker\"\n  version = \"1.0.0\"\n}\n",
	"modules/worker/variables.hcl":  "variable \"config_path\" {}\n",
	"modules/worker/worker.norca.hcl": `job "worker" {
  id          = "riggerit-${rigger.release}"
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

	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())
	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)
	riggerRun(t, exitChanges, []string{"+ config", "+ worker", "+ cache", "redis 0.4.1", "install", "secret riggerit/app", "create", "generate cache, config-token"}, "stack", "plan", dir)
	riggerRun(t, exitOK, []string{"install default/config: done", "install default/worker: done", "install default/cache: done"}, "stack", "apply", dir)
	riggerRun(t, exitOK, []string{"= config", "= worker", "= cache", "nothing to do"}, "stack", "plan", dir)
	checkCluster(t, client)
	riggerRun(t, exitChanges, []string{"~ config", "upgrade", "= worker"}, "stack", "plan", dir, "-var", "greeting=hi")
	riggerRun(t, exitOK, []string{"config", "worker", "in sync"}, "stack", "status", dir)
	prune(t, dir)

	// One release without a stack.
	module := filepath.Join(dir, "modules", "config")
	riggerRun(t, exitOK, []string{"install default/solo: revision 1"}, "install", module, "-name", "solo", "-var", "greeting=x", "-var", "token=y")
	riggerRun(t, exitOK, []string{"default/solo is unchanged"}, "install", module, "-name", "solo", "-var", "greeting=x", "-var", "token=y")
	riggerRun(t, exitOK, []string{"solo", "config 1.0.0", "-"}, "release", "list")
	riggerRun(t, exitOK, []string{"solo"}, "release", "history", "solo")
	riggerRun(t, exitOK, []string{"uninstalled default/solo"}, "release", "uninstall", "solo")
}

// ownedByStack says whether the owner of a revision is the stack of that
// namespace and name. The owner is a field of the release API; the
// description of a release is for people.
func ownedByStack(owner *api.ReleaseOwner, namespace, name string) bool {
	return owner != nil && owner.Kind == api.ReleaseOwnerKindStack && owner.Namespace == namespace && owner.Name == name
}

// checkCluster checks what the apply wrote: the generated token in the
// Variable and the secret, never in the revision; runtime references in
// the job as written.
func checkCluster(t *testing.T, client *api.Client) {
	t.Helper()

	secret, _, err := client.Secrets().Read("riggerit/app", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	token := secret.Items["token"]

	v, _, err := client.Variables().Read("riggerit/config", nil)
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

	if data, _ := json.Marshal(rev); strings.Contains(string(data), token) || !ownedByStack(rev.Spec.Owner, "default", "riggerit") {
		t.Errorf("the revision holds the token or its owner is not the stack default/riggerit: %s", data)
	}

	job, _, err := client.Jobs().Info("riggerit-worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	if env := job.TaskGroups[0].Tasks[0].Env; env["CONFIG"] != "riggerit/config" || env["ALLOC"] != "${NORCA_ALLOC_ID}" {
		t.Errorf("env of the job = %v", env)
	}
}

// prune takes the worker out of the stack: shown, removed only with
// -prune; then destroys the stack.
func prune(t *testing.T, dir string) {
	t.Helper()

	stack := strings.Replace(integrationStack[stackFile], "module \"worker\"", "module \"worker2\"", 1)
	if err := os.WriteFile(filepath.Join(dir, stackFile), []byte(stack), 0o600); err != nil {
		t.Fatal(err)
	}

	riggerRun(t, exitChanges, []string{"- worker", "remove (with -prune)", "+ worker2"}, "stack", "plan", dir)
	riggerRun(t, exitOK, []string{"remove default/worker: done"}, "stack", "apply", dir, "-prune")
	riggerRun(t, exitOK, []string{"uninstalled default/"}, "stack", "destroy", dir)
}
