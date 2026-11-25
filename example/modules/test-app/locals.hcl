# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

locals {
  multiregion = length(var.multiregion) > 0

  # In sidecar mode the application port is not mapped to the host: the
  # client checks it on the address of the allocation. null leaves the
  # attribute out.
  check_address_mode = var.mtls_sidecar ? "alloc" : null

  # The lines of the page that read the secret: a comment of the template
  # and a Vault-style read, which shows only the version.
  secret_lines = [
    "{{- /* A Vault-style read: without a Vault integration it is served by the built-in store (kv/data/<p> reads the secret <p>). Only the version is shown, never the value. */}}",
    "{{ with secret \"kv/data/${var.job_name}/db\" }}<p>Secret ${var.job_name}/db: version {{ .Data.metadata.version }}</p>{{ end }}",
  ]

  # The page of the application. The {{ }} is the template language of the
  # task, filled in on the node when the task starts.
  index_html = join("\n", concat(
    [
      "<!doctype html>",
      "<html>",
      "<head><title>${var.job_name}</title></head>",
      "<body>",
      "<h1>${var.job_name}</h1>",
      "<p>Allocation {{ env \"NORCA_ALLOC_ID\" }} on node {{ env \"node.unique.name\" }}, region {{ env \"NORCA_REGION\" }}, namespace {{ env \"NORCA_NAMESPACE\" }}.</p>",
    ],
    var.secrets_enabled ? local.secret_lines : [],
    [
      "</body>",
      "</html>",
      "",
    ],
  ))
}
