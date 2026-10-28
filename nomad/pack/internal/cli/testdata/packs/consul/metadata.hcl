# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

app {
  url = "https://developer.hashicorp.com/consul"
}

pack {
  name        = "consul"
  description = "Installs and manages the official HashiCorp Consul release (BSL 1.1, not distributed by Norca): servers on host volumes spread over datacenters, a client agent per node, gossip key, TLS and ACL bootstrap from Variables, optional snapshots to S3."
  version     = "0.1.0"
}
