# The Variable of the pack, only with authentication on. Norca's format of a
# Variable: a path and its items.
variable "auth" {
  enabled = var.auth_enabled

  # As the pack: the path does not follow job_name, so a job with another
  # name does not read this Variable by its workload identity (README.md).
  path = "nomad/jobs/redis"

  items {
    mode     = var.mode
    password = var.redis_password # sensitive: a secret item of the release
  }
}
