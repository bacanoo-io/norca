# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
[[- $v := vars . ]]
[[- template "consul.validate" . ]]
[[- $job := $v.job_name ]][[ if eq $job "" ]][[ $job = (meta "pack.name" .) ]][[ end ]]
[[- $tls := $v.tls_enabled ]]
[[- $hn := $v.host_network ]]
[[- $srcVar := printf "nomad/jobs/%s/servers/bootstrap" $job ]]
[[- $serverVar := printf "nomad/jobs/%s/servers" $job ]]
[[- $api := "http" ]][[ if $tls ]][[ $api = "https" ]][[ end ]]
[[- $local := "127.0.0.1" ]][[ if not $tls ]][[ $local = "127.0.0.1 {{ $ip }}" ]][[ end ]]
[[- $dataDir := "/consul/data" ]][[ if eq $v.driver "raw_exec" ]][[ $dataDir = $v.server_data_path ]][[ end ]]

# Consul servers of one Norca region, rendered by the consul pack.
#
# Consul is licensed by HashiCorp under the Business Source License 1.1. Norca
# does not distribute it: the nodes pull the official image or download the
# official release archive referenced below. Nomad does not need Consul to
# run this job, so there is no circular dependency.
job [[ $job | quote ]] {
  [[- template "consul.location" . ]]
  type = "service"

  group "servers" {
    count = [[ $v.server_count ]]

    meta {
      consul_job = [[ $job | quote ]]
    }

    # One server per node: the agents use static ports on the host network.
    constraint {
      operator = "distinct_hosts"
      value    = "true"
    }
    [[- if ne $v.server_node_meta "" ]]

    # Only on the nodes reserved for Consul servers.
    constraint {
      attribute = "${meta.[[ $v.server_node_meta ]]}"
      value     = "true"
    }
    [[- end ]]
    [[- if ne $v.zone_attribute "" ]]

    # Spread the servers over the zones: losing a zone keeps the quorum.
    spread {
      attribute = [[ $v.zone_attribute | quote ]]
      weight    = 100
    }
    [[- if $v.zone_separate_required ]]

    # At most one server per zone (fork feature).
    separate {
      selector {
        consul_job = [[ $job | quote ]]
      }
      scope    = [[ $v.zone_attribute | quote ]]
      required = true
    }
    [[- end ]]
    [[- end ]]

    # Drains and evictions take down one server at a time, so the Raft
    # quorum survives (fork feature).
    disruption {
      max_unavailable = "1"
    }

    # Rolling upgrade, one server at a time: the next server is replaced only
    # after the previous one passed its checks for min_healthy_time. Servers
    # keep their data on the host volume and do not leave the cluster when
    # they stop, so a restarted server rejoins as the same Raft peer.
    update {
      max_parallel      = 1
      health_check      = "checks"
      min_healthy_time  = [[ $v.min_healthy_time | quote ]]
      healthy_deadline  = [[ $v.healthy_deadline | quote ]]
      progress_deadline = "0"
      auto_revert       = false
    }

    migrate {
      max_parallel     = 1
      health_check     = "checks"
      min_healthy_time = [[ $v.min_healthy_time | quote ]]
      healthy_deadline = [[ $v.healthy_deadline | quote ]]
    }
    [[- if ne $v.driver "raw_exec" ]]

    # Raft log and snapshots. The scheduler only places servers on nodes that
    # have this host volume.
    volume "data" {
      type      = "host"
      source    = [[ $v.server_host_volume | quote ]]
      read_only = false
    }
    [[- end ]]

    network {
      [[ template "consul.port" (dict "label" "server" "number" $v.port_server "host_network" $hn) ]]
      [[ template "consul.port" (dict "label" "serf_lan" "number" $v.port_serf_lan "host_network" $hn) ]]
      [[ template "consul.port" (dict "label" "serf_wan" "number" $v.port_serf_wan "host_network" $hn) ]]
      [[ template "consul.port" (dict "label" "http" "number" $v.port_http "host_network" $hn) ]]
      [[ template "consul.port" (dict "label" "grpc" "number" $v.port_grpc "host_network" $hn) ]]
      [[- if $tls ]]
      [[ template "consul.port" (dict "label" "https" "number" $v.port_https "host_network" $hn) ]]
      [[ template "consul.port" (dict "label" "grpc_tls" "number" $v.port_grpc_tls "host_network" $hn) ]]
      [[- end ]]
      [[ template "consul.port" (dict "label" "dns" "number" $v.port_dns "host_network" $hn) ]]
    }

    # Server discovery: the retry_join lists of the servers and the clients
    # follow this group service. It is registered before the tasks start and
    # has no checks, so servers that have not formed the cluster yet are
    # found too.
    service {
      name     = "[[ $job ]]-serf"
      port     = "serf_lan"
      provider = "nomad"
    }

    # Generates the gossip key, the CA, the server certificate and the ACL
    # tokens on the first start and stores them in Variables (see the README),
    # then waits until all servers are registered so that they find each
    # other on the first start.
    task "bootstrap" {
      lifecycle {
        hook    = "prestart"
        sidecar = false
      }

      driver = [[ $v.bootstrap_driver | quote ]]

      config {
        command = "/bin/sh"
        args    = ["${NOMAD_TASK_DIR}/bootstrap.sh"]
      }

      # NOMAD_TOKEN: the workload identity, used through the task API socket.
      identity {
        env = true
      }

      env {
        NORCA_ADDR = "unix://${NOMAD_SECRETS_DIR}/api.sock"
        NORCA_BIN  = [[ $v.norca_binary | quote ]]
      }

      restart {
        attempts = 10
        interval = "30m"
        delay    = "15s"
        mode     = "delay"
      }

      template {
        destination = "local/bootstrap.sh"
        perms       = "0755"
        data        = <<-EOT
          #!/bin/sh
          # Bootstrap of the Consul servers of job [[ $job ]] (consul pack). Runs
          # before every server start; the secrets are generated only once.
          set -eu
          umask 077
          norca="$NORCA_BIN"
          src="[[ $srcVar ]]"
          work="$NOMAD_SECRETS_DIR/bootstrap"
          rm -rf "$work"
          mkdir -p "$work"
          cd "$work"

          if "$norca" var get -item=gossip_key "$src" >/dev/null 2>&1; then
            echo "bootstrap: using the secrets in $src"
          else
            echo "bootstrap: generating the gossip key, CA, server certificate and ACL tokens"
            "$norca" operator gossip keyring generate >gossip_key
            "$norca" tls ca create -days=[[ $v.ca_days ]] >/dev/null
            "$norca" tls cert create -server -region=[[ $v.datacenter | quote ]] -domain=[[ $v.domain | quote ]] \
              -days=[[ $v.cert_days ]] -ca=nomad-agent-ca.pem -key=nomad-agent-ca-key.pem >/dev/null
            mv nomad-agent-ca.pem ca_cert
            mv nomad-agent-ca-key.pem ca_key
            mv "[[ $v.datacenter ]]-server-[[ $v.domain ]].pem" server_cert
            mv "[[ $v.datacenter ]]-server-[[ $v.domain ]]-key.pem" server_key
            for t in management_token agent_token agent_accessor snapshot_token snapshot_accessor; do
              cat /proc/sys/kernel/random/uuid >"$t"
            done
            # Created only if it does not exist: when several servers start at
            # the same time one of them wins, and all continue with its secrets.
            "$norca" var put -out=none -check-index=0 "$src" \
              gossip_key=@gossip_key ca_cert=@ca_cert ca_key=@ca_key \
              server_cert=@server_cert server_key=@server_key \
              management_token=@management_token agent_token=@agent_token agent_accessor=@agent_accessor \
              snapshot_token=@snapshot_token snapshot_accessor=@snapshot_accessor ||
              echo "bootstrap: $src was created by another server, using it"
            rm -f ./*
          fi

          # The Variables the tasks read are derived from the source on every
          # start (the CA key stays in the source only).
          items="gossip_key ca_cert server_cert server_key management_token agent_token agent_accessor snapshot_token snapshot_accessor"
          for item in $items; do
            "$norca" var get -item="$item" "$src" >"$item"
          done
          "$norca" var put -out=none -force "[[ $serverVar ]]" \
            gossip_key=@gossip_key ca_cert=@ca_cert server_cert=@server_cert server_key=@server_key \
            management_token=@management_token agent_token=@agent_token agent_accessor=@agent_accessor \
            snapshot_token=@snapshot_token snapshot_accessor=@snapshot_accessor
[[- if $v.clients_enabled ]]
          "$norca" var put -out=none -force "nomad/jobs/[[ $job ]]-clients" \
            gossip_key=@gossip_key ca_cert=@ca_cert agent_token=@agent_token
[[- end ]]
[[- if $v.snapshot_enabled ]]
          "$norca" var put -out=none -force "nomad/jobs/[[ $job ]]-snapshot/snapshot" \
            consul_token=@snapshot_token
[[- end ]]
          cd /
          rm -rf "$work"

          # Wait (at most 5 minutes) until every server is registered, so that
          # the retry_join lists of the first start contain each other.
          i=0
          while [ "$i" -lt 60 ]; do
            n=$("$norca" service info -json "[[ $job ]]-serf" 2>/dev/null | grep -c '"AllocID"' || true)
            if [ "$n" -ge [[ $v.server_count ]] ]; then
              break
            fi
            echo "bootstrap: $n of [[ $v.server_count ]] servers registered, waiting"
            i=$((i + 1))
            sleep 5
          done
          echo "bootstrap: done"
        EOT
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }

    task "server" {
      [[ template "consul.runtime" (dict "ctx" . "script" "" "args" (list "agent" "-config-dir=${NOMAD_SECRETS_DIR}/consul.d")) ]]
      [[- if ne $v.driver "raw_exec" ]]

      volume_mount {
        volume      = "data"
        destination = "/consul/data"
      }
      [[- end ]]

      kill_timeout = "30s"

      # The configuration. Changes (and new certificates) are applied with a
      # reload; servers stay Raft peers when they stop.
      template {
        destination   = "secrets/consul.d/consul.hcl"
        perms         = "0600"
        change_mode   = "signal"
        change_signal = "SIGHUP"
        data          = <<-EOT
          {{- with nomadVar "[[ $serverVar ]]" }}
          {{- $ip := env "NOMAD_IP_serf_lan" }}
          datacenter       = "[[ $v.datacenter ]]"
          domain           = "[[ $v.domain ]]"
          server           = true
          bootstrap_expect = [[ $v.server_count ]]
          data_dir         = "[[ $dataDir ]]"
          log_level        = "[[ $v.log_level ]]"

          leave_on_terminate      = false
          skip_leave_on_interrupt = true
          disable_update_check    = true
[[ if $tls ]]
          # Gossip and RPC on the private network; plain HTTP and gRPC on
          # localhost only, HTTPS and gRPC-TLS on localhost and the private
          # network. Never on a public network.
[[- else ]]
          # Gossip, RPC and the APIs on localhost and the private network
          # only, never on a public network.
[[- end ]]
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
            server   = [[ $v.port_server ]]
            serf_lan = [[ $v.port_serf_lan ]]
            serf_wan = [[ $v.port_serf_wan ]]
            http     = [[ $v.port_http ]]
            https    = [[ if $tls ]][[ $v.port_https ]][[ else ]]-1[[ end ]]
            grpc     = [[ $v.port_grpc ]]
            grpc_tls = [[ if $tls ]][[ $v.port_grpc_tls ]][[ else ]]-1[[ end ]]
            dns      = [[ $v.port_dns ]]
          }

          ui_config {
            enabled = [[ $v.ui_enabled ]]
          }

          performance {
            raft_multiplier = 1
          }

          encrypt                 = "{{ .gossip_key.Value | trimSpace }}"
          encrypt_verify_incoming = true
          encrypt_verify_outgoing = true

          # Mandatory ACLs, default deny, no anonymous access. The
          # initial_management token bootstraps the ACL system on the first
          # leader election; the acl-setup task creates the agent token.
          acl {
            enabled                  = true
            default_policy           = "deny"
            down_policy              = "extend-cache"
            enable_token_persistence = true
            tokens {
              initial_management = "{{ .management_token.Value | trimSpace }}"
              agent              = "{{ .agent_token.Value | trimSpace }}"
            }
          }
[[- if $tls ]]

          tls {
            defaults {
              ca_file         = "{{ env "NOMAD_SECRETS_DIR" }}/tls/ca_cert.pem"
              cert_file       = "{{ env "NOMAD_SECRETS_DIR" }}/tls/server_cert.pem"
              key_file        = "{{ env "NOMAD_SECRETS_DIR" }}/tls/server_key.pem"
              verify_incoming = true
              verify_outgoing = true
            }
            internal_rpc {
              verify_server_hostname = true
            }
            https {
              verify_incoming = false
            }
            grpc {
              verify_incoming = false
            }
          }

          # The clients get their certificates from the servers.
          auto_encrypt {
            allow_tls = true
          }
[[- end ]]
          {{- end }}
[[- if ne $v.server_extra_config "" ]]

[[ $v.server_extra_config | indent 10 ]]
[[- end ]]
        EOT
      }

      # The other servers, from the <job>-serf service. Consul reads
      # retry_join only at start, so a change needs no action.
      template {
        destination = "secrets/consul.d/join.json"
        perms       = "0600"
        change_mode = "noop"
        data        = <<-EOT
          {"retry_join": [{{ range $i, $s := nomadService "[[ $job ]]-serf" }}{{ if $i }}, {{ end }}"{{ $s.Address }}:{{ $s.Port }}"{{ end }}]}
        EOT
      }
      [[- if $tls ]]
      [[- range $item := list "ca_cert" "server_cert" "server_key" ]]

      template {
        destination   = "secrets/tls/[[ $item ]].pem"
        perms         = "0600"
        change_mode   = "signal"
        change_signal = "SIGHUP"
        data          = "{{ with nomadVar \"[[ $serverVar ]]\" }}{{ .[[ $item ]].Value }}{{ end }}"
      }
      [[- end ]]
      [[- end ]]

      # The API service with the health check roles (fork feature): startup
      # until the RPC port is up, readiness while it answers (rolling updates
      # and drains wait for it), liveness restarts a hung agent.
      service {
        name     = [[ $job | quote ]]
        port     = [[ $api | quote ]]
        provider = "nomad"

        check {
          name            = "startup"
          role            = "startup"
          type            = "tcp"
          port            = "server"
          interval        = "5s"
          timeout         = "2s"
          startup_timeout = "5m"
        }

        check {
          name     = "readiness"
          role     = "readiness"
          type     = "tcp"
          port     = "server"
          interval = "10s"
          timeout  = "2s"
        }

        check {
          name     = "liveness"
          role     = "liveness"
          type     = "tcp"
          port     = [[ $api | quote ]]
          interval = "15s"
          timeout  = "3s"

          check_restart {
            limit = 3
          }
        }
      }

      resources {
        cpu    = [[ $v.server_resources.cpu ]]
        memory = [[ $v.server_resources.memory ]]
      }
    }

    # Creates the agent[[ if $v.snapshot_enabled ]] and snapshot[[ end ]] ACL policy and token with the secrets of the
    # bootstrap task once the cluster has a leader. Idempotent; it runs after
    # every server start and exits.
    task "acl-setup" {
      lifecycle {
        hook    = "poststart"
        sidecar = false
      }

      [[ template "consul.runtime" (dict "ctx" . "script" "${NOMAD_TASK_DIR}/acl-setup.sh" "args" (list)) ]]

      env {
        CONSUL_BIN       = "[[ template "consul.bin" . ]]"
        CONSUL_HTTP_ADDR = "http://127.0.0.1:[[ $v.port_http ]]"
      }

      restart {
        attempts = 10
        interval = "30m"
        delay    = "15s"
        mode     = "delay"
      }
      [[- range $item := list "management_token" "agent_token" "snapshot_token" ]]

      template {
        destination = "secrets/[[ $item ]]"
        perms       = "0600"
        data        = "{{ with nomadVar \"[[ $serverVar ]]\" }}{{ .[[ $item ]].Value | trimSpace }}{{ end }}"
      }
      [[- end ]]

      template {
        destination = "secrets/accessors.env"
        perms       = "0600"
        data        = <<-EOT
          {{- with nomadVar "[[ $serverVar ]]" }}
          AGENT_ACCESSOR={{ .agent_accessor.Value | trimSpace }}
          SNAPSHOT_ACCESSOR={{ .snapshot_accessor.Value | trimSpace }}
          {{- end }}
        EOT
      }

      template {
        destination = "local/agent-policy.hcl"
        data        = <<-EOT
          node_prefix "" {
            policy = "write"
          }
          service_prefix "" {
            policy = "read"
          }
        EOT
      }
      [[- if $v.snapshot_enabled ]]

      template {
        destination = "local/snapshot-policy.hcl"
        data        = <<-EOT
          acl = "write"
        EOT
      }
      [[- end ]]

      template {
        destination = "local/acl-setup.sh"
        perms       = "0755"
        data        = <<-EOT
          #!/bin/sh
          # ACL setup of the Consul servers of job [[ $job ]] (consul pack).
          set -u
          consul="$CONSUL_BIN"
          mgmt="$NOMAD_SECRETS_DIR/management_token"
          . "$NOMAD_SECRETS_DIR/accessors.env"

          until "$consul" operator raft list-peers -token-file="$mgmt" >/dev/null 2>&1; do
            echo "acl-setup: waiting for the cluster to elect a leader"
            sleep 5
          done

          # ensure_policy <name> <description> <rules file>
          ensure_policy() {
            until "$consul" acl policy read -name="$1" -token-file="$mgmt" >/dev/null 2>&1; do
              "$consul" acl policy create -name="$1" -description="$2" -rules="@$3" \
                -token-file="$mgmt" >/dev/null || sleep 3
            done
          }

          # ensure_token <accessor> <secret file> <policy> <description>
          ensure_token() {
            until "$consul" acl token read -self -token-file="$2" >/dev/null 2>&1; do
              "$consul" acl token create -accessor="$1" -secret="$(cat "$2")" -policy-name="$3" \
                -description="$4" -token-file="$mgmt" >/dev/null || sleep 3
            done
          }

          ensure_policy norca-agent "Consul agents installed by the Norca consul pack" \
            "$NOMAD_TASK_DIR/agent-policy.hcl"
          ensure_token "$AGENT_ACCESSOR" "$NOMAD_SECRETS_DIR/agent_token" norca-agent \
            "Agent token of the Consul agents of job [[ $job ]]"
[[- if $v.snapshot_enabled ]]
          ensure_policy norca-snapshot "Snapshots of the Norca consul pack" \
            "$NOMAD_TASK_DIR/snapshot-policy.hcl"
          ensure_token "$SNAPSHOT_ACCESSOR" "$NOMAD_SECRETS_DIR/snapshot_token" norca-snapshot \
            "Snapshot token of job [[ $job ]]-snapshot"
[[- end ]]
          echo "acl-setup: done"
        EOT
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }
}
