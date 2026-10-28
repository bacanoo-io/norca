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
  # Service lookups of the workloads on this client (nomadService in
  # templates, the Task API) return only healthy instances unless they ask
  # with ?healthy=false. Other API calls return every instance unless they
  # ask with ?healthy=true; the DNS server always answers with healthy ones.
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
  # It listens on Consul's default address, which is also the default of the
  # agent's own consul block. The agent notices this and keeps its Consul
  # integration from talking to itself: no auto_advertise, no auto-join, no
  # consul fingerprint (a warning at start says so); templates of tasks read
  # services through this API.
  consul_compat {
    enabled         = true
    bind            = "127.0.0.1:8500"
    datacenter_from = "region"
    namespaces      = ["default"]
  }
}
