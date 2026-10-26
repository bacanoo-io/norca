# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Region eu-1, the authoritative region of the federation: ACL policies and
# tokens, namespaces and quotas are written here and replicated to the
# other regions. Install as /etc/norca.d/region.hcl on the three nodes of
# eu-1, next to norca.hcl and node.hcl of 03-three-node; the agent loads the
# files in lexical order, so this file overrides norca.hcl. Set it before
# the first start: the region of a server can't be changed later.
region = "eu-1"

tls {
  # norca tls cert create -server -region eu-1 ...
  cert_file = "/etc/norca.d/tls/eu-1-server-nomad.pem"
  key_file  = "/etc/norca.d/tls/eu-1-server-nomad-key.pem"
}

server {
  authoritative_region = "eu-1"

  # The servers of all regions share one gossip pool: use the same encrypt
  # key in every region (norca.hcl).

  # The trust domain of native mTLS defaults to "<authoritative region>.nomad",
  # so all regions share it; federation copies the CAs of the other regions
  # into the trust bundle of this one (the default).
  mtls {
    federation = true
  }
}
