// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package template

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	ctconf "github.com/hashicorp/consul-template/config"
)

// The nomadSecret template function (Norca, docs/design/secrets-plan.md,
// "Delivery to tasks"). The function is an external function of
// consul-template served by the task's secrets.TemplateSource. Since
// external functions have no consul-template dependencies, a change of a
// secret is signaled through an internal template of the runner that reads
// a trigger file in the task's private directory: the source rewrites the
// file when a secret changes, consul-template's file watch renders all
// templates again, and the usual change modes apply to the templates whose
// output changed.

// Without a Vault integration, the Vault "secret" and "secrets" functions of
// consul-template are replaced by the source's Vault-style functions (phase
// 4, "Vault compatibility"); their changes re-render the templates through
// the same trigger.

const (
	// secretTemplateFunc is the name of the template function.
	secretTemplateFunc = "nomadSecret"

	// vaultSecretTemplateFunc and vaultSecretsTemplateFunc are the Vault
	// template functions of consul-template.
	vaultSecretTemplateFunc  = "secret"
	vaultSecretsTemplateFunc = "secrets"

	secretTriggerFile   = ".nomad-secrets-trigger"
	secretTriggerOut    = ".nomad-secrets-trigger.rendered"
	secretTriggerSubdir = "private"
)

// secretTriggerPath returns the trigger file of a task directory.
func secretTriggerPath(taskDir string) string {
	return filepath.Join(taskDir, secretTriggerSubdir, secretTriggerFile)
}

// usesNomadSecret reports whether a template calls nomadSecret, or the Vault
// secret function when the source serves it, from its embedded data or its
// source file. A false positive only adds the trigger template.
func usesNomadSecret(config *TaskTemplateManagerConfig) bool {
	name := secretTemplateFunc
	if config.Secrets.ServesVault() {
		// "secret" is part of "nomadSecret" too.
		name = vaultSecretTemplateFunc
	}
	taskEnv := config.EnvBuilder.Build()
	for _, tmpl := range config.Templates {
		if strings.Contains(strings.ToLower(tmpl.EmbeddedTmpl), strings.ToLower(name)) {
			return true
		}
		if tmpl.SourcePath != "" {
			src, _ := taskEnv.ClientPath(tmpl.SourcePath, false)
			if b, err := os.ReadFile(src); err == nil && strings.Contains(strings.ToLower(string(b)), strings.ToLower(name)) {
				return true
			}
		}
	}
	return false
}

// secretTriggerTemplate returns the internal template watching the trigger
// file, nil if no template calls nomadSecret. The trigger file is created.
func secretTriggerTemplate(config *TaskTemplateManagerConfig) (*ctconf.TemplateConfig, error) {
	if config.Secrets == nil || !usesNomadSecret(config) {
		return nil, nil
	}
	trigger := secretTriggerPath(config.TaskDir)
	if err := os.MkdirAll(filepath.Dir(trigger), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create the secret template trigger: %v", err)
	}
	if _, err := os.Stat(trigger); os.IsNotExist(err) {
		if err := os.WriteFile(trigger, []byte("0"), 0o600); err != nil {
			return nil, fmt.Errorf("failed to create the secret template trigger: %v", err)
		}
	}
	contents := fmt.Sprintf("{{ file %q }}", trigger)
	dest := filepath.Join(filepath.Dir(trigger), secretTriggerOut)
	ct := ctconf.DefaultTemplateConfig()
	ct.Contents = &contents
	ct.Destination = &dest
	empty := ""
	ct.Source = &empty
	if !config.ClientConfig.TemplateConfig.DisableSandbox {
		ct.SandboxPath = &config.TaskDir
	}
	ct.Finalize()
	return ct, nil
}

var secretTriggerCounter atomic.Uint64

// relaySecretChanges rewrites the trigger file whenever a secret of a
// nomadSecret call changed.
func (tm *TaskTemplateManager) relaySecretChanges() {
	for {
		select {
		case <-tm.shutdownCh:
			return
		case <-tm.config.Secrets.Changes():
			n := secretTriggerCounter.Add(1)
			tmp := tm.secretTrigger + ".tmp"
			if err := os.WriteFile(tmp, []byte(strconv.FormatUint(n, 10)), 0o600); err == nil {
				_ = os.Rename(tmp, tm.secretTrigger)
			}
		}
	}
}
