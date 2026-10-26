// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"regexp"
	"strings"
)

// Sources of the Vault-style secret lookups of a job (Norca,
// docs/design/secrets-plan.md, "Vault compatibility").
const (
	// SecretRefSourceVault is a task vault block.
	SecretRefSourceVault = "vault"

	// SecretRefSourceVaultTemplate is a Vault "secret" template call.
	SecretRefSourceVaultTemplate = "vault_template"
)

// vaultSecretCall matches the first argument of the Vault "secret" template
// calls (not nomadSecret, not secrets).
var vaultSecretCall = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])secret\s+"([^"]+)"`)

// VaultSecretRefs returns the Vault-style lookups of the job: its task vault
// blocks (Name "vault", no path) and the Vault "secret" calls of its
// embedded templates, with their Vault paths. With a Vault integration they
// go to Vault; without one they resolve on the clients, with the built-in
// store as the fallback.
func (j *Job) VaultSecretRefs() []*SecretRef {
	var out []*SecretRef
	for _, tg := range j.TaskGroups {
		def := j.SecretDefaultProvider(tg)
		for _, t := range tg.Tasks {
			if t.Vault != nil {
				out = append(out, &SecretRef{
					Group:           tg.Name,
					Task:            t.Name,
					Source:          SecretRefSourceVault,
					Name:            "vault",
					DefaultProvider: def,
					Env:             []string{},
				})
			}
			for _, tmpl := range t.Templates {
				if tmpl == nil {
					continue
				}
				for _, m := range vaultSecretCall.FindAllStringSubmatch(tmpl.EmbeddedTmpl, -1) {
					provider, p := ParseSecretRef(m[1])
					p, _, _ = strings.Cut(strings.TrimPrefix(p, "/"), "?")
					out = append(out, &SecretRef{
						Group:           tg.Name,
						Task:            t.Name,
						Source:          SecretRefSourceVaultTemplate,
						Provider:        provider,
						DefaultProvider: def,
						Path:            p,
						Env:             []string{},
					})
				}
			}
		}
	}
	return out
}
