# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Region eu-2, federated with eu-1. Install as /etc/norca.d/region.hcl on
# the three nodes of eu-2, next to norca.hcl and node.hcl of 03-three-node
# (with the eu-2 addresses in node.hcl). Set it before the first start.
region = "eu-2"

tls {
  # norca tls cert create -server -region eu-2 ... (the same CA as eu-1)
  cert_file = "/etc/norca.d/tls/eu-2-server-nomad.pem"
  key_file  = "/etc/norca.d/tls/eu-2-server-nomad-key.pem"
}

acl {
  enabled = true

  # A global management token of the authoritative region:
  #   norca acl token create -region eu-1 -type management -global -name replication-eu-2
  replication_token = "CHANGE_ME"
}

server {
  authoritative_region = "eu-1"

  # The eu-2 servers, plus one server of eu-1: joining it federates the
  # regions (all servers share one gossip pool, with the same encrypt key).
  server_join {
    retry_join = ["10.0.1.11", "10.0.1.12", "10.0.1.13", "10.0.0.11"] # CHANGE_ME
  }

  mtls {
    federation = true
  }
}

client {
  # The clients of eu-2 read the catalog of eu-1 (the gateway of the
  # upstream of the probe job) with this token, which must be valid in
  # every region: a global token with read-job (../policies/acl-catalog-read.hcl).
  #   norca acl token create -region eu-1 -name catalog-read -policy catalog-read -global
  # Without it the clients use their node identity, which other regions
  # don't accept.
  dns {
    token = "CHANGE_ME"
  }
}
