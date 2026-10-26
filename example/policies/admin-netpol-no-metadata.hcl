# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Cluster-wide admin network policy: no workload reaches the metadata
# service of the cloud provider. Admin policies are evaluated before the
# namespace policies, lower priority first; "deny" is final and can't be
# overridden by a namespace policy.
#   norca network-policy apply -admin admin-netpol-no-metadata.hcl
#   norca network-policy list -admin
name        = "no-metadata-service"
description = "Block the cloud metadata service for all workloads"
priority    = 10

# Without a selector the policy selects every allocation of these
# namespaces.
namespaces = ["*"]

egress {
  name   = "metadata"
  action = "deny"

  to {
    cidr = ["169.254.169.254/32"]
  }
}

# Rules are enforced in bridge and CNI networking only. "best_effort" keeps
# placing allocations that can't enforce them (host networking, nodes
# without nftables), with a warning at job registration; "required" (the
# default) would keep every host-mode group of the selected namespaces
# from being placed.
enforcement = "best_effort"
