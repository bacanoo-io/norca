# Locals are named expressions; check blocks are conditions over evaluated
# values, for what a rule over the variables can't say.
#
# What is resolved when: var.*, local.*, module.* and rigger.* (release,
# namespace, region, module.name, module.version) at install time, by
# Rigger. Everything else inside a job (${NORCA_PORT_http}, ${attr.*}, the
# {{ }} of a task template) belongs to the cluster and passes through.

locals {
  # The release name: two releases of the module in one namespace get jobs,
  # services and Variables of their own.
  name = rigger.release

  # A Norca Variable the release writes (settings.var.hcl) and the job
  # reads when it starts; its ACL policy (webapp.acl.hcl) allows that.
  settings_path = "apps/${local.name}/settings"

  # The routing tags of Traefik, only with a route.
  route_tags = var.public ? [
    "traefik.enable=true",
    "traefik.http.routers.${local.name}.rule=Host(`${var.hostname}`)",
    "traefik.http.routers.${local.name}.entrypoints=websecure",
    "traefik.http.routers.${local.name}.tls=true",
  ] : []

  # The configuration file of the application, from a template file in the
  # template syntax of HCL (files/*.tftpl): ${...} writes a value, %{ for }
  # and %{ if } repeat or leave out lines, ~ strips the line end next to
  # it. The template sees only the values given here and can't call
  # templatefile() itself. A plain file is read with file() instead.
  app_config = templatefile("files/app.toml.tftpl", {
    release   = local.name
    namespace = rigger.namespace
    port      = var.port
    features  = var.features
    cache     = var.cache_enabled ? var.cache.service : ""
  })

  # The environment of the task, built from data: only what is configured
  # appears. merge() lets var.env add to it, never override what the
  # module sets.
  env = merge(
    var.env,
    {
      APP_CONFIG         = "${runtime_env("NORCA_TASK_DIR")}/app.toml"
      WHOAMI_NAME        = local.name
      WHOAMI_PORT_NUMBER = tostring(var.port)
    },
    var.cache_enabled ? {
      CACHE_SERVICE = var.cache.service
    } : {},
  )
}

# Stops the install when the instances would reserve more memory than a
# small cluster has: a value only known once the child module evaluated.
check "memory_of_all_instances" {
  condition     = module.sizing.memory * var.count <= 16384
  error_message = "count instances of this size reserve more than 16 GiB of memory; lower count or size."
}
