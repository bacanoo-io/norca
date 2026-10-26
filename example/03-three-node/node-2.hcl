# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Settings of node 2: /etc/norca.d/node.hcl next to norca.hcl.
name = "norca-2"

advertise {
  http = "10.0.0.12" # CHANGE_ME: the address of this node
  rpc  = "10.0.0.12" # CHANGE_ME
  serf = "10.0.0.12" # CHANGE_ME
}

client {
  # Topology zone of the node: the scope of separate blocks
  # (${meta.zone}) and the zone of its service registrations.
  meta {
    zone = "zone-b"
  }
}
