# One Norca Variable: a path and its items, all strings (a number or a bool
# becomes its text). The release writes it; the job reads it at run time
# with variable_ref() (webapp.norca.hcl), allowed by webapp.acl.hcl.
# Not the variable block of variables.hcl: the file extension decides the
# kind, so the two do not meet.

variable "settings" {
  path = local.settings_path

  items {
    log_level = var.log_level
    features  = join(",", sort([for name, on in var.features : name if on]))

    # A sensitive value becomes a secret item of the release: stored in the
    # Variable, never in a revision, and named but not shown by a plan.
    api_token = var.api_token
  }
}
