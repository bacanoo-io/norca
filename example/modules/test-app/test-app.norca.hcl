# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Test application of the Norca examples: nginx with pages rendered by task
# templates. The label is only the name the module refers to the job by;
# the ID comes from job_name.
job "test-app" {
  id          = var.job_name
  name        = var.job_name
  region      = rigger.region
  datacenters = var.datacenters
  type        = "service"

  # The selectors of network policies, colocate/separate blocks and
  # admission policies match this meta.
  meta {
    app  = var.job_name
    team = var.team
  }

  # Multiregion deployment: the job is registered in every region with the
  # same version and rolled out region by region.
  dynamic "multiregion" {
    for_each = local.multiregion ? [1] : []

    content {
      strategy {
        max_parallel = var.multiregion_max_parallel
        on_failure   = var.multiregion_on_failure
      }

      dynamic "region" {
        for_each = var.multiregion
        labels   = [region.value.name]

        content {
          count = region.value.count
        }
      }
    }
  }

  update {
    max_parallel     = 1
    health_check     = "checks"
    min_healthy_time = "10s"
    healthy_deadline = "5m"
    auto_revert      = true
  }

  group "web" {
    # 0 with multiregion: every region uses the count of its region block.
    count = local.multiregion ? 0 : var.count

    network {
      mode = var.network_mode

      # In sidecar mode the application port is not mapped to the host.
      dynamic "port" {
        for_each = var.mtls_sidecar ? [] : ["http"]
        labels   = [port.value]

        content {
          to = 80
        }
      }
    }

    # Disruption budget: drains and evictions (norca alloc stop
    # -respect-disruption) take down at most this many instances at a time.
    dynamic "disruption" {
      for_each = var.disruption_max_unavailable == "" ? [] : [var.disruption_max_unavailable]

      content {
        max_unavailable = disruption.value
      }
    }

    # Cross-job anti-affinity: keep the instances of the job apart.
    dynamic "separate" {
      for_each = var.separate_scope == "" ? [] : [var.separate_scope]

      content {
        selector {
          app = var.job_name
        }

        scope    = separate.value
        required = var.separate_required
      }
    }

    # Typed scaling policy of the built-in autoscaler: keep the average CPU
    # usage of the instances near the target. In a module the policy is
    # written as a value; its check is what a job file writes as the block
    # check "cpu" { ... }.
    dynamic "scaling" {
      for_each = var.scaling_enabled ? [1] : []

      content {
        enabled = true
        engine  = "builtin"
        min     = var.scaling_min
        max     = var.scaling_max

        policy = {
          evaluation_interval     = "30s"
          cooldown                = "2m"
          scale_during_deployment = "up_only"

          check = [{
            cpu = [{
              metric   = "nomad.cpu"
              strategy = "target-value"
              target   = var.scaling_cpu_target
            }]
          }]
        }
      }
    }

    # The tasks of the group may read the secrets below <job>/ of the
    # built-in secret store with their workload identity. The submitter
    # needs read-secret on the pattern. ${job} is the placeholder of Norca's
    # secret paths for the ID of the job, not a value of Rigger: it reaches
    # the job as written.
    dynamic "secret_access" {
      for_each = var.secrets_enabled ? [1] : []

      content {
        path = "${job}/*"
      }
    }

    # Service in the native catalog (also served by the built-in DNS and the
    # Consul-compatible API). The check roles: startup gates the others and
    # restarts the task if it never comes up, readiness decides whether the
    # instance gets traffic and counts for deployments, liveness restarts a
    # hung task.
    service {
      name = var.job_name
      port = var.mtls_sidecar ? "80" : "http"
      tags = ["http"]

      # Envoy sidecar: the service is registered with the address of the
      # sidecar and accepts only mTLS connections allowed by the service
      # intentions of the namespace.
      dynamic "mtls" {
        for_each = var.mtls_sidecar ? [1] : []

        content {
          mode = "sidecar"
        }
      }

      dynamic "discovery" {
        for_each = var.discovery_prefer == "" ? [] : [var.discovery_prefer]

        content {
          prefer    = discovery.value
          min_local = var.discovery_min_local > 0 ? var.discovery_min_local : null
          failover  = length(var.discovery_failover) > 0 ? var.discovery_failover : null
        }
      }

      check {
        name            = "startup"
        role            = "startup"
        type            = "http"
        path            = "/healthz"
        interval        = "5s"
        timeout         = "2s"
        address_mode    = local.check_address_mode
        startup_timeout = var.startup_timeout
      }

      check {
        name         = "readiness"
        role         = "readiness"
        type         = "http"
        path         = "/ready"
        interval     = "10s"
        timeout      = "2s"
        address_mode = local.check_address_mode
      }

      check {
        name         = "liveness"
        role         = "liveness"
        type         = "http"
        path         = "/healthz"
        interval     = "15s"
        timeout      = "2s"
        address_mode = local.check_address_mode
      }
    }

    task "nginx" {
      driver = "docker"

      config {
        image = var.image
        # Docker port mapping in host mode; in bridge mode the port of the
        # group network is enough.
        ports = var.network_mode == "bridge" ? null : ["http"]

        # The pages rendered by the templates below.
        volumes = ["local/html:/usr/share/nginx/html"]
      }

      # A secret of the built-in store, delivered into the secrets directory
      # and into the environment. Jobs only reference secrets; the value
      # never appears in the job, the allocation or the task events.
      dynamic "secret" {
        for_each = var.secrets_enabled ? ["db"] : []
        labels   = [secret.value]

        content {
          path = "${job}/db"

          file {
            destination = "secrets/db.env"
            format      = "env"
          }

          env {
            DB_USER     = "username"
            DB_PASSWORD = "password"
          }

          # nginx reloads on SIGHUP; a new version of the secret is delivered
          # without a restart.
          change_mode   = "signal"
          change_signal = "SIGHUP"
        }
      }

      # The {{ }} below is the task template of the cluster, filled in on
      # the node; Rigger fills in only the name of the job.
      template {
        destination = "local/html/index.html"
        change_mode = "noop"
        data        = local.index_html
      }

      template {
        destination = "local/html/healthz"
        change_mode = "noop"
        data        = "ok\n"
      }

      template {
        destination = "local/html/ready"
        change_mode = "noop"
        data        = "ready\n"
      }

      resources {
        cpu    = var.cpu
        memory = var.memory
      }
    }
  }
}
