# One job: the file name before .norca.hcl is the label of the job block,
# the name the module and its tests refer to it by (resource.job.webapp).
# The format is the job specification of Norca, with expressions in the
# values and the attributes Rigger adds to every resource: enabled,
# count or for_each. An attribute that evaluates to null is left out;
# a dynamic block is written zero or more times.

job "webapp" {
  # The ID of the job: from the release, so that two releases of the module
  # in one namespace do not write one job.
  id   = local.name
  name = local.name
  type = "service"

  region      = rigger.region
  datacenters = var.datacenters
  node_pool   = var.node_pool == "" ? null : var.node_pool

  meta = {
    "app.module"         = rigger.module.name
    "app.module-version" = rigger.module.version
  }

  update {
    max_parallel     = 1
    min_healthy_time = "10s"
    healthy_deadline = "3m"
    auto_revert      = true
  }

  group "web" {
    count = var.count

    # Over the datacenters, with more than one instance. runtime() names a
    # value the cluster fills in on the node.
    dynamic "spread" {
      for_each = var.count > 1 ? [runtime("node.datacenter")] : []

      content {
        attribute = spread.value
        weight    = 100
      }
    }

    network {
      port "http" {
        to = var.port
      }
    }

    # The task may read these secrets of the secret store.
    secret_access {
      path = var.session_secret
    }

    dynamic "secret_access" {
      for_each = var.cache_enabled ? [var.cache.password_secret] : []

      content {
        path = secret_access.value
      }
    }

    service {
      name     = local.name
      port     = "http"
      provider = "nomad"
      tags     = concat(var.service_tags, local.route_tags)

      check {
        name     = "ready"
        role     = "readiness"
        type     = "http"
        path     = var.health_path
        interval = "10s"
        timeout  = "2s"
      }

      check {
        name     = "alive"
        role     = "liveness"
        type     = "http"
        path     = var.health_path
        interval = "30s"
        timeout  = "3s"

        check_restart {
          limit = 3
          grace = "30s"
        }
      }
    }

    task "web" {
      driver = "docker"

      config {
        image = image_ref(var.image, var.image_digest)
        ports = ["http"]
        args  = length(var.args) == 0 ? null : var.args
      }

      env = local.env

      # The configuration file, rendered at install time.
      template {
        destination = "local/app.toml"
        data        = local.app_config
      }

      # Values of run time, read when the task starts: secret_ref() reads an
      # item of the secret store, variable_ref() an item of a Norca
      # Variable, service_address() an address of the service catalog. The
      # task template of the cluster ({{ }}) is written for them; nothing
      # secret is in the job.
      template {
        destination = "secrets/app.env"
        env         = true
        data        = <<-EOT
          SESSION_KEY=${secret_ref(var.session_secret, "key", { quote = false })}
          LOG_LEVEL=${variable_ref(local.settings_path, "log_level", { quote = false })}
          API_TOKEN=${variable_ref(local.settings_path, "api_token", { quote = false })}
        EOT
      }

      dynamic "template" {
        for_each = var.cache_enabled ? [var.cache] : []

        content {
          destination = "secrets/cache.env"
          env         = true
          data        = <<-EOT
            CACHE_ADDR=${service_address(template.value.service)}
            CACHE_PASSWORD=${secret_ref(template.value.password_secret, "password", { quote = false })}
          EOT
        }
      }

      resources {
        cpu        = module.sizing.cpu
        memory     = module.sizing.memory
        memory_max = module.sizing.memory_max
      }
    }
  }
}
