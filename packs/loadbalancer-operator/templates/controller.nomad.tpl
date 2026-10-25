[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
job [[ $v.job_name | quote ]] {
  type        = "service"
  namespace   = [[ $v.namespace | quote ]]
[[- if ne $v.region "" ]]
  region      = [[ $v.region | quote ]]
[[- end ]]
  datacenters = [[ $v.datacenters | toStringList ]]

  group "controller" {
    count = [[ $v.count ]]

    network {
      port "metrics" {}
[[- if $v.webhook_enabled ]]
      port "webhook" {
        static = [[ $v.webhook_port ]]
      }
[[- end ]]
    }

    task "controller" {
      driver = [[ $v.driver | quote ]]

      config {
[[- if eq $v.driver "mock_driver" ]]
        run_for = "87600h"
[[- else if eq $v.driver "docker" ]]
        image = [[ $v.image | quote ]]
        ports = ["metrics"]
        args  = [
[[- else ]]
        command = [[ $v.binary | quote ]]
        args = [
[[- end ]]
[[- if ne $v.driver "mock_driver" ]]
          "controller",
          "-objects", "native",
          "-namespace", [[ $v.namespace | quote ]],
          "-log-level", [[ $v.log_level | quote ]],
          "-discover-services=[[ $v.discover_services ]]",
          "-proxy-driver", [[ $v.proxy_driver | quote ]],
[[- if eq $v.proxy_driver "docker" ]]
          "-proxy-image", [[ $v.proxy_image | quote ]],
[[- else if ne $v.proxy_driver "mock_driver" ]]
          "-proxy-command", [[ $v.binary | quote ]],
[[- end ]]
          "-metrics-addr", "${NOMAD_ADDR_metrics}",
        ]
[[- end ]]
      }

      # The controller calls the API on the task API socket with its
      # workload identity; the ACL policy of the pack is bound to this job.
      identity {
        env = true
      }

      env {
        NORCA_ADDR = "unix://${NOMAD_SECRETS_DIR}/api.sock"
      }

      resources {
        cpu    = [[ $v.cpu ]]
        memory = [[ $v.memory ]]
      }
    }
[[- if $v.webhook_enabled ]]

    # The defaulting and validating webhooks of the LoadBalancer kind. The
    # certificate and key come from the job's Variable (nomad/jobs/<job>,
    # items webhook_cert and webhook_key).
    task "webhook" {
      driver = [[ $v.driver | quote ]]

      config {
[[- if eq $v.driver "mock_driver" ]]
        run_for = "87600h"
[[- else if eq $v.driver "docker" ]]
        image = [[ $v.image | quote ]]
        ports = ["webhook"]
        args  = [
[[- else ]]
        command = [[ $v.binary | quote ]]
        args = [
[[- end ]]
[[- if ne $v.driver "mock_driver" ]]
          "webhook",
          "-listen", ":${NOMAD_PORT_webhook}",
          "-tls-cert", "${NOMAD_SECRETS_DIR}/webhook.pem",
          "-tls-key", "${NOMAD_SECRETS_DIR}/webhook-key.pem",
          "-classes", [[ join "," $v.classes | quote ]],
          "-default-class", [[ $v.default_class | quote ]],
        ]
[[- end ]]
      }
[[- if ne $v.driver "mock_driver" ]]

      template {
        data        = "{{ with nomadVar \"nomad/jobs/[[ $v.job_name ]]\" }}{{ .webhook_cert }}{{ end }}"
        destination = "secrets/webhook.pem"
        change_mode = "restart"
      }

      template {
        data        = "{{ with nomadVar \"nomad/jobs/[[ $v.job_name ]]\" }}{{ .webhook_key }}{{ end }}"
        destination = "secrets/webhook-key.pem"
        change_mode = "restart"
      }
[[- end ]]

      service {
        name     = "[[ $v.job_name ]]-webhook"
        port     = "webhook"
        provider = "nomad"
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
[[- end ]]
  }
}
