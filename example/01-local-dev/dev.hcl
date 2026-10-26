# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Additions to a development agent (server and client in one process, data
# in memory, no ACLs, no TLS):
#   norca agent -dev -config dev.hcl
# (as root if jobs use bridge networking)

server {
  # Built-in autoscaler on the leader; scaling policies without an engine
  # use it.
  autoscaler {
    enabled        = true
    default_engine = "builtin"
  }
}

client {
  # The topology zone of the node (the default source is ${meta.zone}).
  meta {
    zone = "dev"
  }

  # Discovery (nomadService, the API without ?healthy, DNS) returns only
  # healthy instances by default.
  native_service_discovery {
    default_healthy_only = true
  }

  # Built-in DNS server answering from the native catalog:
  #   dig @127.0.0.1 -p 8600 test-app.service.nomad SRV
  dns {
    enabled       = true
    bind          = "127.0.0.1:8600"
    consul_compat = true # *.service.consul names as well
    ttl           = "5s"
  }

  # Read-only Consul-compatible HTTP API for tools that only know Consul
  # (Traefik consulcatalog, Prometheus consul_sd_configs, consul-template):
  #   curl http://127.0.0.1:8500/v1/catalog/services
  consul_compat {
    enabled         = true
    bind            = "127.0.0.1:8500"
    datacenter_from = "region"
    namespaces      = ["default"]
  }
}

# Norca does not need Consul. The consul_compat API listens on Consul's
# default address 127.0.0.1:8500, so keep the agent's own Consul integration
# from registering the agent there.
consul {
  auto_advertise   = false
  server_auto_join = false
  client_auto_join = false
}
