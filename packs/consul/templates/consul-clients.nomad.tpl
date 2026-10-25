[[- $v := vars . -]]
[[- if $v.clients_enabled -]]
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
[[- template "consul.validate" . ]]
[[- $job := $v.job_name ]][[ if eq $job "" ]][[ $job = (meta "pack.name" .) ]][[ end ]]
[[- $tls := $v.tls_enabled ]]
[[- $hn := $v.host_network ]]
[[- $clientVar := printf "nomad/jobs/%s-clients" $job ]]
[[- $local := "127.0.0.1" ]][[ if not $tls ]][[ $local = "127.0.0.1 {{ $ip }}" ]][[ end ]]

# Consul client agents of the [[ $job ]] datacenter, one per node, rendered by
# the consul pack. Consul is the official HashiCorp release (BSL 1.1), not
# distributed by Norca. Workloads reach their local agent on 127.0.0.1.
job "[[ $job ]]-clients" {
[[- template "consul.location" . ]]
  type = "system"

  group "client" {
[[- if ne $v.server_node_meta "" ]]
    # Not on the server nodes: a server is the local agent of its node.
    constraint {
      attribute = "${meta.[[ $v.server_node_meta ]]}"
      operator  = "!="
      value     = "true"
    }
[[- end ]]

    update {
      max_parallel     = 1
      min_healthy_time = "10s"
    }

    # The agent state (node ID, check state) survives restarts on the node.
    ephemeral_disk {
      sticky = true
      size   = 300
    }

    network {
[[ template "consul.port" (dict "label" "serf_lan" "number" $v.port_serf_lan "host_network" $hn) ]]
[[ template "consul.port" (dict "label" "http" "number" $v.port_http "host_network" $hn) ]]
[[ template "consul.port" (dict "label" "grpc" "number" $v.port_grpc "host_network" $hn) ]]
[[- if $tls ]]
[[ template "consul.port" (dict "label" "https" "number" $v.port_https "host_network" $hn) ]]
[[ template "consul.port" (dict "label" "grpc_tls" "number" $v.port_grpc_tls "host_network" $hn) ]]
[[- end ]]
[[ template "consul.port" (dict "label" "dns" "number" $v.port_dns "host_network" $hn) ]]
    }

    task "client" {
      [[ template "consul.runtime" (dict "ctx" . "script" "" "args" (list "agent" "-config-dir=${NOMAD_SECRETS_DIR}/consul.d")) ]]

      kill_timeout = "30s"

      # The configuration, from the Variable written by the servers'
      # bootstrap task (the task waits until it exists).
      template {
        destination   = "secrets/consul.d/consul.hcl"
        perms         = "0600"
        change_mode   = "signal"
        change_signal = "SIGHUP"
        data          = <<-EOT
          {{- with nomadVar "[[ $clientVar ]]" }}
          {{- $ip := env "NOMAD_IP_serf_lan" }}
          datacenter = "[[ $v.datacenter ]]"
          domain     = "[[ $v.domain ]]"
          server     = false
          data_dir   = "{{ env "NOMAD_ALLOC_DIR" }}/data/consul"
          log_level  = "[[ $v.log_level ]]"

          leave_on_terminate   = true
          disable_update_check = true

          bind_addr      = "{{ $ip }}"
          advertise_addr = "{{ $ip }}"
          client_addr    = "127.0.0.1"
          addresses {
            http     = "[[ $local ]]"
            grpc     = "[[ $local ]]"
[[- if $tls ]]
            https    = "127.0.0.1 {{ $ip }}"
            grpc_tls = "127.0.0.1 {{ $ip }}"
[[- end ]]
            dns      = "[[ $v.dns_address ]]"
          }
          ports {
            serf_lan = [[ $v.port_serf_lan ]]
            http     = [[ $v.port_http ]]
            https    = [[ if $tls ]][[ $v.port_https ]][[ else ]]-1[[ end ]]
            grpc     = [[ $v.port_grpc ]]
            grpc_tls = [[ if $tls ]][[ $v.port_grpc_tls ]][[ else ]]-1[[ end ]]
            dns      = [[ $v.port_dns ]]
          }

          encrypt                 = "{{ .gossip_key.Value | trimSpace }}"
          encrypt_verify_incoming = true
          encrypt_verify_outgoing = true

          acl {
            enabled                  = true
            default_policy           = "deny"
            down_policy              = "extend-cache"
            enable_token_persistence = true
            tokens {
              agent = "{{ .agent_token.Value | trimSpace }}"
            }
          }
[[- if $tls ]]

          # Only the CA: the client certificate comes from the servers.
          tls {
            defaults {
              ca_file         = "{{ env "NOMAD_SECRETS_DIR" }}/tls/ca_cert.pem"
              verify_incoming = false
              verify_outgoing = true
            }
            internal_rpc {
              verify_server_hostname = true
            }
          }

          auto_encrypt {
            tls = true
          }
[[- end ]]
          {{- end }}
[[- if ne $v.client_extra_config "" ]]

[[ $v.client_extra_config | indent 10 ]]
[[- end ]]
        EOT
      }

      # The servers, from the <job>-serf service. retry_join is read only at
      # start: a client that started before the servers were registered is
      # restarted (with a random delay) when the list changes.
      template {
        destination = "secrets/consul.d/join.json"
        perms       = "0600"
        change_mode = "restart"
        splay       = "2m"
        data        = <<-EOT
          {"retry_join": [{{ range $i, $s := nomadService "[[ $job ]]-serf" }}{{ if $i }}, {{ end }}"{{ $s.Address }}:{{ $s.Port }}"{{ end }}]}
        EOT
      }
[[- if $tls ]]

      template {
        destination   = "secrets/tls/ca_cert.pem"
        perms         = "0600"
        change_mode   = "signal"
        change_signal = "SIGHUP"
        data          = "{{ with nomadVar \"[[ $clientVar ]]\" }}{{ .ca_cert.Value }}{{ end }}"
      }
[[- end ]]

      service {
        name     = "[[ $job ]]-client"
        port     = "serf_lan"
        provider = "nomad"

        check {
          name     = "liveness"
          role     = "liveness"
          type     = "tcp"
          port     = "serf_lan"
          interval = "15s"
          timeout  = "3s"

          check_restart {
            limit = 3
          }
        }
      }

      resources {
        cpu    = [[ $v.client_resources.cpu ]]
        memory = [[ $v.client_resources.memory ]]
      }
    }
  }
}
[[- end ]]
