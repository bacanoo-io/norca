# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Settings of node 1: /etc/norca.d/node.hcl next to norca.hcl.
name = "norca-1"

# The datacenter of the node: one datacenter per availability zone. It is the
# scope of the separate blocks (${node.datacenter}) and the first tier of
# topology-aware discovery.
datacenter = "dc-a"

advertise {
  http = "10.0.0.11" # CHANGE_ME: the address of this node
  rpc  = "10.0.0.11" # CHANGE_ME
  serf = "10.0.0.11" # CHANGE_ME
}

client {
  # The data of OpenObserve (step 12): only this node has the volume, so the
  # single instance always runs here.
  host_volume "openobserve" {
    path      = "/srv/openobserve"
    read_only = false
  }
}
