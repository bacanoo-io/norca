policy "backup" {
  enabled = var.backup.enabled

  name        = "${local.job}-backup"
  description = "The dump job of ${local.job} may read the service catalog."

  # Bound to the job: its tasks get these rights with their workload identity.
  job_acl {
    job_id = "${local.job}-backup"
  }

  # The rules as a block: Rigger writes it as the text of the policy. The
  # namespace is the one of the release; a label of HCL is a literal, so it
  # is given by a dynamic block.
  rules {
    dynamic "namespace" {
      for_each = [rigger.namespace]
      labels   = [namespace.value]

      content {
        capabilities = ["read-job"]
      }
    }
  }
}
