policy "backup" {
  enabled = var.backup.enabled

  name        = "${local.job}-backup"
  description = "The dump job of ${local.job} may read the service catalog."

  # Bound to the job: its tasks get these rights with their workload identity.
  job_acl {
    job_id = "${local.job}-backup"
  }

  rules = <<-EOT
    namespace "${rigger.namespace}" {
      capabilities = ["read-job"]
    }
  EOT
}
