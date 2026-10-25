// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import "github.com/hashicorp/nomad/nomad/structs"

// admissionSecretRefsValue is the "secrets" list of the CEL request
// variable of job admission (Norca, docs/design/secrets-plan.md, "Delivery
// to tasks"): every secret the job references, from its secret blocks and
// the nomadSecret calls of its embedded templates, with the placeholders of
// the paths replaced. Each entry has:
//
//   - group, task, source ("secret" or "template"), name
//   - provider: the explicit provider, "" if the client resolves it
//   - default_provider: the group, job or namespace default
//   - effective_provider: provider, else default_provider ("" if the
//     client routes or the fallback decide)
//   - path, version, file_format ("" without a file), env (the variable
//     names), optional
//
// Example rules: request.secrets.all(s, s.path.startsWith("prod/")),
// request.secrets.all(s, s.effective_provider == "bao"),
// request.secrets.all(s, size(s.env) == 0).
func admissionSecretRefsValue(job *structs.Job) []any {
	refs := job.SecretRefs()
	out := make([]any, 0, len(refs))
	for _, r := range refs {
		env := make([]any, 0, len(r.Env))
		for _, e := range r.Env {
			env = append(env, e)
		}
		effective := r.Provider
		if effective == "" {
			effective = r.DefaultProvider
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
