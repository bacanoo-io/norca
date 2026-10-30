# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The event stream -> NATS bridge, one job per region.
#
#   1. Build the binary for the nodes and install it on them:
#        cd nomad/natsbridge && go build -o /usr/local/bin/norca-natsbridge ./cmd/norca-natsbridge
#   2. Bind the ACL policy to this job (natsbridge-policy.hcl).
#   3. Optional: store the NATS credentials in the job's Variable, they are
#      rendered into the secrets directory by the template below:
#        norca var put nomad/jobs/natsbridge nats_creds=@bridge.creds
#   4. norca job run -var nats_url=nats://nats.service.example:4222 natsbridge.nomad.hcl
#
# Two instances run; leader election over a Variables lease
# (nomad/jobs/natsbridge/lease) makes one of them publish while the other
# stands by. The leader keeps the index of the last published event in
# nomad/jobs/natsbridge/checkpoint, so whichever instance leads next resumes
# from there.
#
# The bridge reaches the API on the task API socket with its workload
# identity (NORCA_TOKEN falls back to the NOMAD_TOKEN set by
# identity { env = true }); the region of the subjects is the region of the
# job (NOMAD_REGION).

variable "binary" {
  type    = string
  default = "/usr/local/bin/norca-natsbridge"
}

variable "nats_url" {
  type    = string
  default = "nats://127.0.0.1:4222"
}

variable "subject_prefix" {
  type    = string
  default = "nomad"
}

variable "topics" {
  type    = string
  default = "Job,Deployment,Evaluation,Allocation,Node,Service,Scaling"
}

# Name of a JetStream stream for the subjects of the region; empty publishes
# with core NATS.
variable "jetstream_stream" {
  type    = string
  default = "NORCA_EVENTS"
}

job "natsbridge" {
  type = "service"

  group "bridge" {
    count = 2

    # The instances on different nodes, so a node failure leaves a
    # standby.
    constraint {
      operator = "distinct_hosts"
      value    = "true"
    }

    network {
      port "metrics" {}
    }

    task "bridge" {
      driver = "raw_exec"

      config {
        command = var.binary
        args = [
          "-nats-url", var.nats_url,
          "-subject-prefix", var.subject_prefix,
          "-topics", var.topics,
          "-metrics-addr", "${NOMAD_ADDR_metrics}",
        ]
      }

      identity {
        env = true
      }

      env {
        NORCA_ADDR = "unix://${NOMAD_SECRETS_DIR}/api.sock"

        # JetStream: wait for the acknowledgements, deduplicate redeliveries
        # (Nats-Msg-Id) and create the stream if it does not exist.
        NATSBRIDGE_JETSTREAM        = var.jetstream_stream != "" ? "true" : "false"
        NATSBRIDGE_JETSTREAM_STREAM = var.jetstream_stream
        NATSBRIDGE_JETSTREAM_CREATE = var.jetstream_stream != "" ? "true" : "false"
        # Limits of the stream: without them it grows until the server's
        # max_file_store is full.
        NATSBRIDGE_JETSTREAM_MAX_AGE     = "72h"
        NATSBRIDGE_JETSTREAM_MAX_BYTES   = "512MB"
        NATSBRIDGE_JETSTREAM_COMPRESSION = "s2"
      }

      # NATS credentials from the job's Variable (see step 3). The Variable
      # is read through nomadVarList, which does not block when it does not
      # exist; without a nats_creds item NATS_CREDS stays unset.
      template {
        data        = <<-EOT
          {{- range nomadVarList "nomad/jobs/natsbridge" }}{{ if eq .Path "nomad/jobs/natsbridge" }}
          {{- with nomadVar .Path }}{{ with .nats_creds }}{{ . }}{{ end }}{{ end }}
          {{- end }}{{ end -}}
        EOT
        destination = "secrets/nats.creds"
        perms       = "0600"
      }
      template {
        data        = <<-EOT
          {{- range nomadVarList "nomad/jobs/natsbridge" }}{{ if eq .Path "nomad/jobs/natsbridge" }}
          {{- with nomadVar .Path }}{{ with .nats_creds }}NATS_CREDS={{ env "NOMAD_SECRETS_DIR" }}/nats.creds{{ end }}{{ end }}
          {{- end }}{{ end -}}
        EOT
        destination = "secrets/nats.env"
        env         = true
      }

      # A stopped leader stores its checkpoint and releases the lease.
      kill_timeout = "20s"

      service {
        name     = "natsbridge-metrics"
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
        cpu    = 100
        memory = 64
      }
    }
  }
}
