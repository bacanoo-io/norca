# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
[[- $v := vars . ]]
[[- template "nats.validate" . ]]
[[- $job := $v.job_name ]][[ if eq $job "" ]][[ $job = (meta "pack.name" .) ]][[ end ]]
[[- $secrets := $v.secrets_variable ]][[ if eq $secrets "" ]][[ $secrets = printf "nomad/jobs/%s" $job ]][[ end ]]
[[- $docker := eq $v.driver "docker" ]]
[[- $clustered := gt $v.count 1 ]]
[[- $hn := $v.host_network ]]

# NATS cluster of one Norca region, rendered by the nats pack. In a
# multi-cloud setup the pack runs once per region; the gateways join the
# clusters into a supercluster over the VPN.
job [[ $job | quote ]] {
[[- template "nats.location" . ]]
  type = "service"

  group "nats" {
    count = [[ $v.count ]]

    meta {
      nats_job = [[ $job | quote ]]
    }
[[- if $v.distinct_hosts ]]

    # One server per node.
    constraint {
      operator = "distinct_hosts"
      value    = "true"
    }
[[- end ]]
[[- if ne $v.separate_scope "" ]]

    # Spread the servers over topology domains (fork feature).
    separate {
      selector {
        nats_job = [[ $job | quote ]]
      }
      scope    = [[ $v.separate_scope | quote ]]
      required = [[ $v.separate_required ]]
    }
[[- end ]]
[[- if ne $v.disruption_max_unavailable "" ]]

    # Drains and evictions take down at most this many servers at a time, so
    # the JetStream Raft groups keep their majority (fork feature).
    disruption {
      max_unavailable = [[ $v.disruption_max_unavailable | quote ]]
    }
[[- end ]]

    # Rolling updates one server at a time; a server counts as healthy only
    # when its readiness check (/healthz: JetStream current) passes.
    update {
      max_parallel      = 1
      health_check      = "checks"
      min_healthy_time  = "10s"
      healthy_deadline  = [[ $v.healthy_deadline | quote ]]
      progress_deadline = "0"
      auto_revert       = false
    }

    migrate {
      max_parallel     = 1
      health_check     = "checks"
      min_healthy_time = "10s"
      healthy_deadline = [[ $v.healthy_deadline | quote ]]
    }
[[- if eq $v.storage "ephemeral_disk" ]]

    # JetStream data in alloc/data: kept when the allocation is replaced on
    # the same node (sticky) and copied to a new node if the old one is still
    # reachable (migrate). Not a durable volume: replication (R3 streams)
    # protects against losing a node.
    ephemeral_disk {
      sticky  = true
      migrate = true
      size    = [[ $v.ephemeral_disk_size ]]
    }
[[- end ]]
[[- if eq $v.storage "host_volume" ]]

    volume "jetstream" {
      type      = "host"
      source    = [[ $v.host_volume | quote ]]
      read_only = false
    }
[[- end ]]

    network {
[[ template "nats.port" (dict "label" "client" "number" $v.client_port "host_network" $hn) ]]
[[- if or $clustered $v.gateway_enabled ]]
[[ template "nats.port" (dict "label" "cluster" "number" $v.cluster_port "host_network" $hn) ]]
[[- end ]]
[[- if $v.gateway_enabled ]]
[[ template "nats.port" (dict "label" "gateway" "number" $v.gateway_port "host_network" $hn) ]]
[[- end ]]
[[- if $v.leafnodes_enabled ]]
[[ template "nats.port" (dict "label" "leafnode" "number" $v.leafnode_port "host_network" $hn) ]]
[[- end ]]
[[ template "nats.port" (dict "label" "monitor" "number" $v.monitor_port "host_network" $hn) ]]
    }
[[- if or $clustered $v.gateway_enabled ]]

    # The route addresses of the servers. A group service is registered
    # before the tasks start, so the servers find each other on their first
    # start; it has no checks, so servers that are not ready yet are found too.
    service {
      name     = "[[ $job ]]-cluster"
      port     = "cluster"
      provider = "nomad"
    }
[[- end ]]
[[- if $v.gateway_enabled ]]

    service {
      name     = "[[ $job ]]-gateway"
      port     = "gateway"
      provider = "nomad"
    }
[[- end ]]

    task "nats" {
      driver = [[ $v.driver | quote ]]

      config {
[[- if $docker ]]
        image = [[ $v.image | quote ]]
        args  = ["--config", "${NOMAD_SECRETS_DIR}/nats.conf"]
        ports = ["client", [[ if or $clustered $v.gateway_enabled ]]"cluster", [[ end ]][[ if $v.gateway_enabled ]]"gateway", [[ end ]][[ if $v.leafnodes_enabled ]]"leafnode", [[ end ]]"monitor"]
[[- else ]]
        command = [[ $v.binary | quote ]]
        args    = ["--config", "${NOMAD_SECRETS_DIR}/nats.conf"]
[[- end ]]
      }
[[- if and (not $docker) (ne $v.artifact_url "") ]]

      artifact {
        source = [[ $v.artifact_url | quote ]]
[[- if ne $v.artifact_checksum "" ]]
        options {
          checksum = [[ $v.artifact_checksum | quote ]]
        }
[[- end ]]
      }
[[- end ]]
[[- if eq $v.storage "host_volume" ]]

      volume_mount {
        volume      = "jetstream"
        destination = "/data"
      }
[[- end ]]

      # Lame duck mode: stop accepting clients, move them to the other
      # servers, then shut down.
      kill_signal  = "SIGUSR2"
      kill_timeout = [[ $v.kill_timeout | quote ]]

      # The configuration. Routes follow the <job>-cluster service; changes
      # (and new certificates or credentials) are applied with a reload.
      template {
        destination   = "secrets/nats.conf"
        perms         = "0600"
        change_mode   = "signal"
        change_signal = "SIGHUP"
        data          = <<-EOT
          {{- $name := [[ if $v.cluster_name ]][[ $v.cluster_name | quote ]][[ else ]]env "NOMAD_REGION"[[ end ]] }}
          server_name: "{{ $name }}-{{ env "NOMAD_ALLOC_INDEX" }}"
          listen: "[[ if $docker ]]0.0.0.0[[ else ]]{{ env "NOMAD_IP_client" }}[[ end ]]:{{ env "NOMAD_PORT_client" }}"
          client_advertise: "{{ env "NOMAD_ADDR_client" }}"
          http: "[[ if $docker ]]0.0.0.0[[ else ]]{{ env "NOMAD_IP_monitor" }}[[ end ]]:{{ env "NOMAD_PORT_monitor" }}"
          lame_duck_duration: "[[ $v.lame_duck_duration ]]"
          lame_duck_grace_period: "5s"
[[- if $v.debug ]]
          debug: true
[[- end ]]
[[- if $v.tls_enabled ]]

          tls {
            ca_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/ca.pem"
            cert_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/cert.pem"
            key_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/key.pem"
            verify: [[ $v.tls_verify_clients ]]
            timeout: 5
          }
[[- end ]]
[[- if ne $v.auth_mode "none" ]]

          {{- with nomadVar "[[ $secrets ]]" }}
[[- if eq $v.auth_mode "token" ]]
          authorization {
            token: {{ .client_token.Value | toJSON }}
          }
[[- else if eq $v.auth_mode "nkey" ]]
          authorization {
            users: [
            {{- range split "," (.client_nkeys.Value | replaceAll "\n" ",") }}{{ with trimSpace . }}
              { nkey: {{ toJSON . }} }
            {{- end }}{{ end }}
            ]
          }
[[- else ]]
          {{ .auth_config.Value }}
[[- end ]]
          {{- end }}
[[- end ]]
[[- if $v.jetstream_enabled ]]

          jetstream {
[[- if eq $v.storage "ephemeral_disk" ]]
            store_dir: "{{ env "NOMAD_ALLOC_DIR" }}/data"
[[- else if eq $v.storage "host_volume" ]]
            store_dir: "/data/{{ $name }}-{{ env "NOMAD_ALLOC_INDEX" }}"
[[- else ]]
            store_dir: "[[ $v.store_path ]]/{{ $name }}-{{ env "NOMAD_ALLOC_INDEX" }}"
[[- end ]]
            max_memory_store: [[ $v.jetstream_max_memory ]]
            max_file_store: [[ $v.jetstream_max_file ]]
[[- if ne $v.jetstream_domain "" ]]
            domain: [[ $v.jetstream_domain | quote ]]
[[- end ]]
          }
[[- end ]]
[[- if or $clustered $v.gateway_enabled ]]

          cluster {
            name: "{{ $name }}"
            listen: "[[ if $docker ]]0.0.0.0[[ else ]]{{ env "NOMAD_IP_cluster" }}[[ end ]]:{{ env "NOMAD_PORT_cluster" }}"
            advertise: "{{ env "NOMAD_ADDR_cluster" }}"
            # The own address is always listed (the server ignores a route
            # to itself): JetStream clustering needs at least one route.
            routes: [
              "nats-route://{{ env "NOMAD_ADDR_cluster" }}"
            {{- range nomadService "[[ $job ]]-cluster" }}{{ if ne .AllocID (env "NOMAD_ALLOC_ID") }}
              "nats-route://{{ .Address }}:{{ .Port }}"
            {{- end }}{{ end }}
            ]
[[- if $v.tls_enabled ]]
            tls {
              ca_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/ca.pem"
              cert_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/cert.pem"
              key_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/key.pem"
              verify: true
              timeout: 5
            }
[[- end ]]
          }
[[- end ]]
[[- if $v.gateway_enabled ]]

          gateway {
            name: "{{ $name }}"
            listen: "[[ if $docker ]]0.0.0.0[[ else ]]{{ env "NOMAD_IP_gateway" }}[[ end ]]:{{ env "NOMAD_PORT_gateway" }}"
            advertise: "{{ env "NOMAD_ADDR_gateway" }}"
            gateways: [
[[- range $v.remote_gateways ]]
              { name: [[ .name | quote ]], urls: [ [[ range $i, $u := .urls ]][[ if $i ]], [[ end ]][[ $u | quote ]][[ end ]] ] }
[[- end ]]
            ]
[[- if $v.tls_enabled ]]
            tls {
              ca_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/ca.pem"
              cert_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/cert.pem"
              key_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/key.pem"
              verify: true
              timeout: 5
            }
[[- end ]]
          }
[[- end ]]
[[- if $v.leafnodes_enabled ]]

          leafnodes {
            listen: "[[ if $docker ]]0.0.0.0[[ else ]]{{ env "NOMAD_IP_leafnode" }}[[ end ]]:{{ env "NOMAD_PORT_leafnode" }}"
            advertise: "{{ env "NOMAD_ADDR_leafnode" }}"
[[- if $v.tls_enabled ]]
            tls {
              ca_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/ca.pem"
              cert_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/cert.pem"
              key_file: "{{ env "NOMAD_SECRETS_DIR" }}/tls/key.pem"
              verify: [[ $v.tls_verify_clients ]]
              timeout: 5
            }
[[- end ]]
          }
[[- end ]]
[[- if ne $v.extra_config "" ]]

[[ $v.extra_config | indent 10 ]]
[[- end ]]
        EOT
      }
[[- if $v.tls_enabled ]]

      # TLS material from the secrets Variable (items tls_ca, tls_cert,
      # tls_key). The task does not start until the Variable exists.
[[- range $item := list "ca" "cert" "key" ]]
      template {
        destination   = "secrets/tls/[[ $item ]].pem"
        perms         = "0600"
        change_mode   = "signal"
        change_signal = "SIGHUP"
        data          = "{{ with nomadVar \"[[ $secrets ]]\" }}{{ .tls_[[ $item ]] }}{{ end }}"
      }
[[- end ]]
[[- end ]]

      # Client service with the health check roles (fork feature): startup
      # until the server runs with JetStream, readiness while JetStream is
      # current (deployments and drains wait for it), liveness restarts a
      # hung server.
      service {
        name     = [[ $job | quote ]]
        port     = "client"
        provider = "nomad"
[[- $light := "/healthz" ]]
[[- if $v.jetstream_enabled ]][[ $light = "/healthz?js-enabled-only=true" ]][[ end ]]

        check {
          name            = "startup"
          role            = "startup"
          type            = "http"
          port            = "monitor"
          path            = [[ $light | quote ]]
          interval        = "5s"
          timeout         = "2s"
          startup_timeout = [[ $v.startup_timeout | quote ]]
        }

        check {
          name     = "readiness"
          role     = "readiness"
          type     = "http"
          port     = "monitor"
          path     = "/healthz"
          interval = "10s"
          timeout  = "3s"
        }

        check {
          name     = "liveness"
          role     = "liveness"
          type     = "http"
          port     = "monitor"
          path     = [[ $light | quote ]]
          interval = "15s"
          timeout  = "3s"

          check_restart {
            limit = 3
          }
        }
      }

      service {
        name     = "[[ $job ]]-monitor"
        port     = "monitor"
        provider = "nomad"
      }
[[- if $v.leafnodes_enabled ]]

      service {
        name     = "[[ $job ]]-leafnode"
        port     = "leafnode"
        provider = "nomad"
      }
[[- end ]]

      resources {
        cpu    = [[ $v.resources.cpu ]]
        memory = [[ $v.resources.memory ]]
      }
    }
  }
}
