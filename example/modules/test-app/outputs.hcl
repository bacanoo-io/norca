# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

output "service" {
  value       = var.job_name
  description = "Name of the service in the catalog."
}

output "notes" {
  description = "How to check the application."
  value = join("\n", concat(
    [
      "Test application \"${var.job_name}\" in namespace ${rigger.namespace}, image ${var.image}, network ${var.network_mode}${var.mtls_sidecar ? " with an mTLS sidecar" : ""}.",
    ],
    var.secrets_enabled ? [
      "",
      "The task waits for the secret ${var.job_name}/db (items username and password):",
      "  norca secret put -namespace ${rigger.namespace} ${var.job_name}/db username=app password=-",
    ] : [],
    [
      "",
      "Check it:",
      "  norca job status -namespace ${rigger.namespace} ${var.job_name}          # disruption budget, allocations",
      "  norca service info -namespace ${rigger.namespace} -verbose ${var.job_name}  # instances and check results",
    ],
    var.scaling_enabled ? [
      "  norca scaling policy list -namespace ${rigger.namespace} -job ${var.job_name}",
    ] : [],
    [""],
  ))
}
