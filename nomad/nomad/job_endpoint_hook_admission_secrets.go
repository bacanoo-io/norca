// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import "github.com/hashicorp/nomad/nomad/structs"

// admissionSecretRefsValue is the "secrets" list of the CEL request
// variable of job admission (Norca, norca-doc content/docs/design/secrets-plan.md, "Delivery
// to tasks"): every secret the job references, from its secret blocks and
// the nomadSecret calls of its embedded templates, with the placeholders of
// the paths replaced, and its Vault-style lookups (phase 4): the task vault
// blocks and the Vault secret calls of embedded templates. Each entry has:
//
//   - group, task, source ("secret", "template", "vault" for a vault block,
//     "vault_template" for a Vault secret call), name ("vault" for vault
//     blocks)
//   - provider: the explicit provider, "" if the client resolves it
//   - default_provider: the group, job or namespace default
//   - effective_provider: provider, else default_provider; for Vault-style
//     lookups then "vault" when the servers have a Vault integration ("" if
//     the client routes or the fallback decide)
//   - path, version, file_format ("" without a file), env (the variable
//     names), optional
//
// Example rules: request.secrets.all(s, s.path.startsWith("prod/")),
// request.secrets.all(s, s.effective_provider == "bao"),
// request.secrets.all(s, size(s.env) == 0). A namespace can require a
// provider for the Vault-style lookups served by the fallback as well:
// request.secrets.all(s, s.effective_provider != "").
func admissionSecretRefsValue(job *structs.Job, vaultEnabled bool) []any {
	refs := job.SecretRefs()
	vaultRefs := job.VaultSecretRefs()
	out := make([]any, 0, len(refs)+len(vaultRefs))
	for _, r := range append(refs, vaultRefs...) {
		env := make([]any, 0, len(r.Env))
		for _, e := range r.Env {
			env = append(env, e)
		}
		effective := r.Provider
		if effective == "" {
			effective = r.DefaultProvider
		}
		vaultStyle := r.Source == structs.SecretRefSourceVault || r.Source == structs.SecretRefSourceVaultTemplate
		if effective == "" && vaultStyle && vaultEnabled {
			effective = structs.SecretProviderVault
		}
		out = append(out, map[string]any{
			"group":              r.Group,
			"task":               r.Task,
			"source":             r.Source,
			"name":               r.Name,
			"provider":           r.Provider,
			"default_provider":   r.DefaultProvider,
			"effective_provider": effective,
			"path":               r.Path,
			"version":            int64(r.Version),
			"file_format":        r.FileFormat,
			"env":                env,
			"optional":           r.Optional,
		})
	}
	return out
}
