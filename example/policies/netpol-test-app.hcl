# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Network policy of the test application (level A: egress rules, enforced
# with nftables in the network namespace of every selected allocation):
#   norca network-policy apply netpol-test-app.hcl
#   norca network-policy status -namespace apps test-app
#
# The allocations selected by the policy may only open connections to the
# node network of the cluster and to DNS servers; everything else they send
# is dropped. Replies to allowed connections and the client's health checks
# are always allowed. Ingress is not isolated, because the policy has no
# ingress rules.
name        = "test-app"
namespace   = "apps"
description = "test-app: egress only to the cluster network and DNS"

# Matches the job meta of the test-app pack (app = "<job_name>").
selector {
  app = "test-app"
}

egress {
  to {
    cidr = ["10.0.0.0/24"] # CHANGE_ME: the subnet of the nodes
  }
}

egress {
  ports = ["53/udp", "53/tcp"]
}

# "required": the allocations are only placed on nodes that can enforce the
# rules (bridge networking and nftables, node attribute
# norca.network_policy.nftables = true).
enforcement = "required"
