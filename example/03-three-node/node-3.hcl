# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Settings of node 3: /etc/norca.d/node.hcl next to norca.hcl.
name = "norca-3"

# The datacenter of the node: one datacenter per availability zone. It is the
# scope of the separate blocks (${node.datacenter}) and the first tier of
# topology-aware discovery.
datacenter = "dc-c"

advertise {
  http = "10.0.0.13" # CHANGE_ME: the address of this node
  rpc  = "10.0.0.13" # CHANGE_ME
  serf = "10.0.0.13" # CHANGE_ME
}
