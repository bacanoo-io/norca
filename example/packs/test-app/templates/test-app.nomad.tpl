# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
[[- template "test_app.validate" . ]]
[[- $v := vars . ]]
[[- $job := $v.job_name ]]
[[- $bridge := eq $v.network_mode "bridge" ]]
[[- $sidecar := $v.mtls_sidecar ]]

# Test application rendered by the test-app pack of the Norca examples.
job [[ $job | quote ]] {
[[- if ne $v.namespace "" ]]
  namespace   = [[ $v.namespace | quote ]]
[[- end ]]
[[- if ne $v.region "" ]]
  region      = [[ $v.region | quote ]]
[[- end ]]
  datacenters = [[ $v.datacenters | toStringList ]]
  type        = "service"

  # The selectors of network policies, colocate/separate blocks and
  # admission policies match this meta.
  meta {
    app  = [[ $job | quote ]]
    team = [[ $v.team | quote ]]
  }
[[- if $v.multiregion ]]

  # Multiregion deployment: the job is registered in every region with the
  # same version and rolled out region by region.
  multiregion {
    strategy {
      max_parallel = [[ $v.multiregion_max_parallel ]]
      on_failure   = [[ $v.multiregion_on_failure | quote ]]
    }
[[- range $r := $v.multiregion ]]

    region [[ $r.name | quote ]] {
      count = [[ $r.count ]]
    }
[[- end ]]
  }
[[- end ]]

  update {
    max_parallel     = 1
    health_check     = "checks"
    min_healthy_time = "10s"
    healthy_deadline = "5m"
    auto_revert      = true
  }

  group "web" {
[[- if $v.multiregion ]]
    # 0: every region uses the count of its multiregion region.
    count = 0
[[- else ]]
    count = [[ $v.count ]]
[[- end ]]

    network {
      mode = [[ $v.network_mode | quote ]]
[[- if not $sidecar ]]

      port "http" {
        to = 80
      }
[[- end ]]
    }
[[- if ne $v.disruption_max_unavailable "" ]]

    # Disruption budget: drains and evictions (norca alloc stop
    # -respect-disruption) take down at most this many instances at a time.
    disruption {
      max_unavailable = [[ $v.disruption_max_unavailable | quote ]]
    }
[[- end ]]
[[- if ne $v.separate_scope "" ]]

    # Cross-job anti-affinity: keep the instances of the job apart.
    separate {
      selector {
        app = [[ $job | quote ]]
      }
      scope    = [[ $v.separate_scope | quote ]]
      required = [[ $v.separate_required ]]
    }
[[- end ]]
[[- if $v.scaling_enabled ]]

    # Typed scaling policy of the built-in autoscaler: keep the average CPU
    # usage of the instances near the target.
    scaling {
      enabled = true
      engine  = "builtin"
      min     = [[ $v.scaling_min ]]
      max     = [[ $v.scaling_max ]]

      policy {
        evaluation_interval     = "30s"
        cooldown                = "2m"
        scale_during_deployment = "up_only"

        check "cpu" {
          metric   = "nomad.cpu"
          strategy = "target-value"
          target   = [[ $v.scaling_cpu_target ]]
        }
      }
    }
[[- end ]]
[[- if $v.secrets_enabled ]]

    # The tasks of the group may read the secrets below <job>/ of the
    # built-in secret store with their workload identity. The submitter
    # needs read-secret on the pattern.
    secret_access {
      path = "${job}/*"
    }
[[- end ]]

    # Service in the native catalog (also served by the built-in DNS and the
    # Consul-compatible API). The check roles: startup gates the others and
    # restarts the task if it never comes up, readiness decides whether the
    # instance gets traffic and counts for deployments, liveness restarts a
    # hung task.
    service {
      name     = [[ $job | quote ]]
      provider = "nomad"
[[- if $sidecar ]]
      port     = "80"
[[- else ]]
      port     = "http"
[[- end ]]
      tags     = ["http"]
[[- if $sidecar ]]

      # Envoy sidecar: the service is registered with the address of the
      # sidecar and accepts only mTLS connections allowed by the service
      # intentions of the namespace.
      mtls {
        mode = "sidecar"
      }
[[- end ]]
[[- if ne $v.discovery_prefer "" ]]

      discovery {
        prefer = [[ $v.discovery_prefer | quote ]]
[[- if gt (int $v.discovery_min_local) 0 ]]
        min_local = [[ $v.discovery_min_local ]]
[[- end ]]
[[- if $v.discovery_failover ]]
        failover = [[ $v.discovery_failover | toStringList ]]
[[- end ]]
      }
[[- end ]]

      [[ template "test_app.check" (list "startup" "/healthz" "5s" $sidecar $v.startup_timeout) ]]

      [[ template "test_app.check" (list "readiness" "/ready" "10s" $sidecar "") ]]

      [[ template "test_app.check" (list "liveness" "/healthz" "15s" $sidecar "") ]]
    }

    task "nginx" {
      driver = "docker"

      config {
        image = [[ $v.image | quote ]]
[[- if not $bridge ]]
        ports = ["http"]
[[- end ]]

        # The pages rendered by the templates below.
        volumes = ["local/html:/usr/share/nginx/html"]
      }
[[- if $v.secrets_enabled ]]

      # A secret of the built-in store, delivered into the secrets directory
      # and into the environment. Jobs only reference secrets; the value
      # never appears in the job, the allocation or the task events.
      secret "db" {
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
[[- end ]]

      template {
        destination = "local/html/index.html"
        change_mode = "noop"
        data        = <<EOT
<!doctype html>
<html>
<head><title>[[ $job ]]</title></head>
<body>
<h1>[[ $job ]]</h1>
<p>Allocation {{ env "NOMAD_ALLOC_ID" }} on node {{ env "node.unique.name" }}, region {{ env "NOMAD_REGION" }}, namespace {{ env "NOMAD_NAMESPACE" }}.</p>
[[- if $v.secrets_enabled ]]
{{- /* A Vault-style read: without a Vault integration it is served by the built-in store (kv/data/<p> reads the secret <p>). Only the version is shown, never the value. */}}
{{ with secret "kv/data/[[ $job ]]/db" }}<p>Secret [[ $job ]]/db: version {{ .Data.metadata.version }}</p>{{ end }}
[[- end ]]
</body>
</html>
EOT
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
        cpu    = [[ $v.cpu ]]
        memory = [[ $v.memory ]]
      }
    }
  }
}
