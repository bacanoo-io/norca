# One ACL policy, in the format Norca reads: name, description, rules and
# job_acl. The rules are written as an HCL block, not as a string, so that
# rigger fmt formats them and a dynamic block can name a namespace that is
# a value (a block label of HCL is a literal).

policy "webapp" {
  # Policies are of the cluster, not of a namespace: the name carries the
  # namespace and the release.
  name        = "${rigger.namespace}-${local.name}"
  description = "The tasks of ${local.name} may read the jobs of their namespace and their settings Variable."

  rules {
    dynamic "namespace" {
      for_each = [rigger.namespace]
      labels   = [namespace.value]

      content {
        capabilities = ["read-job"]

        variables {
          dynamic "path" {
            for_each = [local.settings_path]
            labels   = [path.value]

            content {
              capabilities = ["read"]
            }
          }
        }
      }
    }
  }

  # Bound to the job: its tasks get these rights with their workload
  # identity, nobody else does.
  job_acl {
    namespace = rigger.namespace
    job_id    = local.name
  }
}
