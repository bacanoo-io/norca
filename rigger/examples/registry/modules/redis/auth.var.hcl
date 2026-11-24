# The Variable with the password, only with authentication on. Norca's
# format of a Variable: a path and its items. The job reads it when it
# starts (locals.hcl); an application that may read the path finds the user
# and the password of AUTH there.
variable "auth" {
  enabled = var.auth_enabled

  path = local.auth_path

  items {
    user     = "default"          # the user of Redis the password belongs to
    password = var.redis_password # sensitive: a secret item of the release
  }
}
