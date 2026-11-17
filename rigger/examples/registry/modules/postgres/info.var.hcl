# What the release recorded about itself, for people and for other tools.
# Not a secret: only names.
variable "info" {
  path = "nomad/jobs/${local.job}/info"

  items = {
    service   = local.job
    port      = tostring(var.port)
    databases = join(",", [for d in var.databases : d.name])
  }
}
