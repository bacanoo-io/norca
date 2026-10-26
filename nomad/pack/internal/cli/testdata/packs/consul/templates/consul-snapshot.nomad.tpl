[[- $v := vars . -]]
[[- if $v.snapshot_enabled -]]
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
[[- template "consul.validate" . ]]
[[- $job := $v.job_name ]][[ if eq $job "" ]][[ $job = (meta "pack.name" .) ]][[ end ]]
[[- $snap := printf "%s-snapshot" $job ]]

# Periodic Consul snapshot of the [[ $job ]] datacenter, rendered by the consul
# pack: consul snapshot save on a server node (against its local agent), then
# an upload to the S3-compatible store of the Norca backups.
job [[ $snap | quote ]] {
  [[- template "consul.location" . ]]
  type = "batch"

  periodic {
    cron             = [[ $v.snapshot_cron | quote ]]
    time_zone        = "UTC"
    prohibit_overlap = true
  }

  group "snapshot" {
    # On a server node: the snapshot is taken through its local agent.
    constraint {
      attribute = "${meta.[[ $v.server_node_meta ]]}"
      value     = "true"
    }

    restart {
      attempts = 2
      delay    = "30s"
      mode     = "fail"
    }

    reschedule {
      attempts  = 1
      interval  = "1h"
      unlimited = false
    }

    task "save" {
      lifecycle {
        hook    = "prestart"
        sidecar = false
      }

      [[ template "consul.runtime" (dict "ctx" . "script" "${NOMAD_TASK_DIR}/save.sh" "args" (list)) ]]

      env {
        CONSUL_BIN       = "[[ template "consul.bin" . ]]"
        CONSUL_HTTP_ADDR = "http://127.0.0.1:[[ $v.port_http ]]"
      }

      # The snapshot token, written by the servers' bootstrap task.
      template {
        destination = "secrets/consul_token"
        perms       = "0600"
        data        = "{{ with nomadVar \"nomad/jobs/[[ $snap ]]/snapshot\" }}{{ .consul_token.Value | trimSpace }}{{ end }}"
      }

      template {
        destination = "local/save.sh"
        perms       = "0755"
        data        = <<-EOT
          #!/bin/sh
          set -eu
          out="$NOMAD_ALLOC_DIR/data/consul.snap"
          "$CONSUL_BIN" snapshot save -token-file="$NOMAD_SECRETS_DIR/consul_token" "$out"
          "$CONSUL_BIN" snapshot inspect "$out"
        EOT
      }

      resources {
        cpu    = 200
        memory = 256
      }
    }

    task "upload" {
      driver = "docker"

      config {
        image      = [[ $v.snapshot_upload_image | quote ]]
        entrypoint = ["/bin/sh"]
        args       = ["${NOMAD_TASK_DIR}/upload.sh"]
      }

      env {
        AWS_CONFIG_FILE = "${NOMAD_SECRETS_DIR}/aws/config"
        [[- if $v.snapshot_s3_credentials ]]
        AWS_SHARED_CREDENTIALS_FILE = "${NOMAD_SECRETS_DIR}/aws/credentials"
        [[- end ]]
      }

      template {
        destination = "secrets/aws/config"
        perms       = "0600"
        data        = <<-EOT
          [default]
          region = [[ $v.snapshot_s3_region ]]
[[- if $v.snapshot_s3_path_style ]]
          s3 =
            addressing_style = path
[[- end ]]
        EOT
      }
      [[- if $v.snapshot_s3_credentials ]]

      # The S3 credentials, from the Variable nomad/jobs/[[ $snap ]].
      template {
        destination = "secrets/aws/credentials"
        perms       = "0600"
        data        = <<-EOT
          {{- with nomadVar "nomad/jobs/[[ $snap ]]" }}
          [default]
          aws_access_key_id = {{ .aws_access_key_id.Value | trimSpace }}
          aws_secret_access_key = {{ .aws_secret_access_key.Value | trimSpace }}
          {{- end }}
        EOT
      }
      [[- end ]]

      template {
        destination = "local/upload.sh"
        perms       = "0755"
        data        = <<-EOT
          #!/bin/sh
          set -eu
          key="[[ trimSuffix "/" $v.snapshot_s3_prefix ]]/[[ $v.datacenter ]]/$(date -u +%Y%m%dT%H%M%SZ).snap"
          aws s3 cp "$NOMAD_ALLOC_DIR/data/consul.snap" "s3://[[ $v.snapshot_s3_bucket ]]/$key"[[ if ne $v.snapshot_s3_endpoint "" ]] \
            --endpoint-url "[[ $v.snapshot_s3_endpoint ]]"[[ end ]][[ if ne $v.snapshot_s3_sse "" ]] \
            --sse "[[ $v.snapshot_s3_sse ]]"[[ end ]]
          rm -f "$NOMAD_ALLOC_DIR/data/consul.snap"
          echo "snapshot: uploaded s3://[[ $v.snapshot_s3_bucket ]]/$key"
        EOT
      }

      resources {
        cpu    = 200
        memory = 256
      }
    }
  }
}
[[- end ]]
