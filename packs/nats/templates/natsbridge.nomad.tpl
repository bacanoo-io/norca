[[- $v := vars . -]]
[[- if $v.bridge_enabled -]]
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
[[- $job := $v.job_name ]][[ if eq $job "" ]][[ $job = (meta "pack.name" .) ]][[ end ]]
[[- $bridge := $v.bridge_job_name ]]
[[- $docker := eq $v.bridge_driver "docker" ]]

# Event stream -> NATS bridge of the region, rendered by the nats pack
# (based on nomad/natsbridge/examples/natsbridge.nomad.hcl). It needs the ACL
# policy printed by the pack bound to this job.
#
# The instances elect a leader over the Variable nomad/jobs/[[ $bridge ]]/lease;
# the leader keeps its position in nomad/jobs/[[ $bridge ]]/checkpoint.
# Optional NATS credentials are read from the Variable nomad/jobs/[[ $bridge ]]:
# nats_token, nats_creds, nats_nkey_seed, nats_ca, nats_cert, nats_key.
job [[ $bridge | quote ]] {
[[- template "nats.location" . ]]
  type = "service"

  group "bridge" {
    count = [[ $v.bridge_count ]]
[[- if $v.distinct_hosts ]]

    # The instances on different nodes, so a node failure leaves a standby.
    constraint {
      operator = "distinct_hosts"
      value    = "true"
    }
[[- end ]]

    network {
      port "metrics" {}
    }

    task "bridge" {
      driver = [[ $v.bridge_driver | quote ]]

      config {
[[- if $docker ]]
        image = [[ $v.bridge_image | quote ]]
        ports = ["metrics"]
        args  = ["-metrics-addr", "0.0.0.0:${NOMAD_PORT_metrics}"]
[[- else ]]
        command = [[ $v.bridge_binary | quote ]]
        args    = ["-metrics-addr", "${NOMAD_ADDR_metrics}"]
[[- end ]]
      }
[[- if and (not $docker) (ne $v.bridge_artifact_url "") ]]

      artifact {
        source = [[ $v.bridge_artifact_url | quote ]]
      }
[[- end ]]

      # The workload identity is the API token (NOMAD_TOKEN); the API is
      # reached on the task API socket.
      identity {
        env = true
      }

      env {
        NORCA_ADDR                  = "unix://${NOMAD_SECRETS_DIR}/api.sock"
        NATSBRIDGE_SUBJECT_PREFIX   = [[ $v.bridge_subject_prefix | quote ]]
        NATSBRIDGE_TOPICS           = [[ $v.bridge_topics | quote ]]
[[- if and $v.jetstream_enabled (ne $v.bridge_jetstream_stream "") ]]
        # JetStream: wait for the acknowledgements, deduplicate redeliveries
        # (Nats-Msg-Id), create the stream for <prefix>.<region>.> if missing.
        NATSBRIDGE_JETSTREAM        = "true"
        NATSBRIDGE_JETSTREAM_STREAM = [[ $v.bridge_jetstream_stream | quote ]]
        NATSBRIDGE_JETSTREAM_CREATE = "true"
[[- end ]]
      }

      # The servers of the region from the [[ $job ]] service, and the optional
      # credentials from the bridge's Variable. nomadVarList does not block
      # when the Variable does not exist. A change of the servers restarts
      # the bridge (it resumes from its checkpoint; JetStream drops the
      # duplicates).
      template {
        destination = "secrets/nats.env"
        env         = true
        change_mode = "restart"
        splay       = "10s"
        data        = <<-EOT
          NATS_URL={{ range $i, $s := nomadService "[[ $job ]]" }}{{ if $i }},{{ end }}nats://{{ $s.Address }}:{{ $s.Port }}{{ end }}
          {{- range nomadVarList "nomad/jobs/[[ $bridge ]]" }}{{ if eq .Path "nomad/jobs/[[ $bridge ]]" }}{{ with nomadVar .Path }}
          {{- with .nats_token.Value }}
          NATS_TOKEN={{ toJSON . }}
          {{- end }}
          {{- if .nats_creds.Value }}
          NATS_CREDS={{ env "NOMAD_SECRETS_DIR" }}/nats/creds
          {{- end }}
          {{- if .nats_nkey_seed.Value }}
          NATS_NKEY={{ env "NOMAD_SECRETS_DIR" }}/nats/nkey.seed
          {{- end }}
          {{- if .nats_ca.Value }}
          NATS_CA={{ env "NOMAD_SECRETS_DIR" }}/nats/ca.pem
          {{- end }}
          {{- if and .nats_cert.Value .nats_key.Value }}
          NATS_CERT={{ env "NOMAD_SECRETS_DIR" }}/nats/cert.pem
          NATS_KEY={{ env "NOMAD_SECRETS_DIR" }}/nats/key.pem
          {{- end }}
          {{- end }}{{ end }}{{ end }}
        EOT
      }
[[- range $item := list "creds" "nkey_seed" "ca" "cert" "key" ]]
      template {
        destination = "secrets/nats/[[ if eq $item "creds" ]]creds[[ else if eq $item "nkey_seed" ]]nkey.seed[[ else ]][[ $item ]].pem[[ end ]]"
        perms       = "0600"
        change_mode = "restart"
        data        = "{{ range nomadVarList \"nomad/jobs/[[ $bridge ]]\" }}{{ if eq .Path \"nomad/jobs/[[ $bridge ]]\" }}{{ with nomadVar .Path }}{{ .nats_[[ $item ]].Value }}{{ end }}{{ end }}{{ end }}"
      }
[[- end ]]

      # A stopped leader stores its checkpoint and releases the lease.
      kill_timeout = "20s"

      service {
        name     = "[[ $bridge ]]-metrics"
        port     = "metrics"
        provider = "nomad"

        check {
          type     = "http"
          path     = "/healthz"
          interval = "10s"
          timeout  = "2s"
        }
      }

      resources {
        cpu    = [[ $v.bridge_resources.cpu ]]
        memory = [[ $v.bridge_resources.memory ]]
      }
    }
  }
}
[[- end ]]
