[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]

[[- /* region, namespace, datacenters and node_pool of a job. */ -]]
[[- define "consul.location" -]]
[[- $v := vars . ]]
[[- if ne $v.region "" ]]
  region      = [[ $v.region | quote ]]
[[- end ]]
[[- if ne $v.namespace "" ]]
  namespace   = [[ $v.namespace | quote ]]
[[- end ]]
[[- if $v.datacenters ]]
  datacenters = [[ $v.datacenters | toStringList ]]
[[- end ]]
[[- if ne $v.node_pool "" ]]
  node_pool   = [[ $v.node_pool | quote ]]
[[- end ]]
[[- end -]]

[[- /* A static network port, bound to host_network if set. */ -]]
[[- define "consul.port" -]]
      port [[ .label | quote ]] {
        static = [[ .number ]]
[[- if ne .host_network "" ]]
        host_network = [[ .host_network | quote ]]
[[- end ]]
      }
[[- end -]]

[[- /* Reference of the official image: <registry>/<repository>:<tag>[@<digest>]. */ -]]
[[- define "consul.image" -]]
[[- $v := vars . -]]
[[- $tag := $v.image_tag ]][[ if eq $tag "" ]][[ $tag = $v.consul_version ]][[ end -]]
[[- $ref := printf "%s:%s" $v.image_repository $tag ]][[ if ne $v.image_registry "" ]][[ $ref = printf "%s/%s" $v.image_registry $ref ]][[ end -]]
[[- if ne $v.image_digest "" ]][[ $ref = printf "%s@%s" $ref $v.image_digest ]][[ end -]]
[[- $ref | quote -]]
[[- end -]]

[[- /* URL of the official release archive (exec, raw_exec). */ -]]
[[- define "consul.download_url" -]]
[[- $v := vars . -]]
[[- printf "%s/%s/consul_%s_linux_%s.zip" (trimSuffix "/" $v.download_base_url) $v.consul_version $v.consul_version $v.download_arch | quote -]]
[[- end -]]

[[- /* The consul binary as seen by a script task. */ -]]
[[- define "consul.bin" -]]
[[- $v := vars . -]]
[[- if eq $v.driver "docker" -]]consul
[[- else if ne $v.consul_binary "" -]][[ $v.consul_binary ]]
[[- else -]]${NOMAD_TASK_DIR}/consul
[[- end -]]
[[- end -]]

[[- /*
Driver, config and artifact of a task running Consul (call it indented on its own line): the agent itself
(.script empty, .args the agent arguments) or a shell script using the consul
CLI (.script the script path). Docker tasks use the host network: the agent is
the node's local agent on 127.0.0.1, and script tasks reach it there. Exec
tasks run as root because the rendered secrets are only readable by root.
*/ -]]
[[- define "consul.runtime" -]]
[[- $v := vars .ctx -]]
driver = [[ $v.driver | quote ]]
[[- if eq $v.driver "exec" ]]
      user   = "root"
[[- end ]]

      config {
[[- if eq $v.driver "docker" ]]
        image        = [[ template "consul.image" .ctx ]]
        network_mode = "host"
[[- if .script ]]
        entrypoint   = ["/bin/sh"]
        args         = [[ list .script | toStringList ]]
[[- else ]]
        # The binary directly, not the image's entrypoint script: the agent
        # runs as root in the container and reads the root-only secrets.
        entrypoint   = ["consul"]
        args         = [[ .args | toStringList ]]
[[- end ]]
[[- else ]]
[[- if .script ]]
        command = "/bin/sh"
        args    = [[ list .script | toStringList ]]
[[- else ]]
        command = [[ if ne $v.consul_binary "" ]][[ $v.consul_binary | quote ]][[ else ]]"local/consul"[[ end ]]
        args    = [[ .args | toStringList ]]
[[- end ]]
[[- end ]]
      }
[[- if and (ne $v.driver "docker") (eq $v.consul_binary "") ]]

      # The official release archive, unpacked into local/.
      artifact {
        source      = [[ template "consul.download_url" .ctx ]]
        destination = "local"
[[- if ne $v.download_checksum "" ]]
        options {
          checksum = [[ $v.download_checksum | quote ]]
        }
[[- end ]]
      }
[[- end ]]
[[- end -]]

[[- /* Validation of the variables. */ -]]
[[- define "consul.validate" -]]
[[- $v := vars . -]]
[[- if not $v.consul_license_accepted -]]
[[- fail "Consul is licensed by HashiCorp under the Business Source License 1.1 and is not distributed by Norca. Read the license at github.com/hashicorp/consul/blob/main/LICENSE and set consul_license_accepted = true to accept it for your use" -]]
[[- end -]]
[[- if not (has $v.driver (list "docker" "exec" "raw_exec")) -]]
[[- fail (printf "driver must be docker, exec or raw_exec, got %q" $v.driver) -]]
[[- end -]]
[[- if not (has $v.bootstrap_driver (list "exec" "raw_exec")) -]]
[[- fail (printf "bootstrap_driver must be exec or raw_exec, got %q" $v.bootstrap_driver) -]]
[[- end -]]
[[- if not (has (int $v.server_count) (list 1 3 5 7)) -]]
[[- fail (printf "server_count must be 3 or 5 (1 for development, 7 at most), got %v" $v.server_count) -]]
[[- end -]]
[[- if and (eq $v.driver "raw_exec") (eq $v.server_data_path "") -]]
[[- fail "raw_exec cannot mount host volumes, so set server_data_path or use the docker or exec driver" -]]
[[- end -]]
[[- if eq $v.consul_version "" -]]
[[- fail "consul_version must be set" -]]
[[- end -]]
[[- if and (ne $v.image_digest "") (not (hasPrefix "sha256:" $v.image_digest)) -]]
[[- fail (printf "image_digest must be sha256 followed by the hex digest, got %q" $v.image_digest) -]]
[[- end -]]
[[- if $v.snapshot_enabled -]]
[[- if eq $v.snapshot_s3_bucket "" -]]
[[- fail "snapshot_enabled needs snapshot_s3_bucket" -]]
[[- end -]]
[[- if eq $v.server_node_meta "" -]]
[[- fail "snapshot_enabled needs server_node_meta (the snapshot job runs on a server node and talks to its local agent)" -]]
[[- end -]]
[[- end -]]
[[- end -]]
