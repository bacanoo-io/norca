# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Variables of the test-app pack on the three-node cluster:
#   norca pack run ../packs/test-app --name=test-app --namespace=apps -f test-app.hcl
count = 3

# Spread the instances over the datacenters (one per availability zone). A
# preference, not a rule: with more instances than datacenters (the
# autoscaler may go up to 6) a required rule would keep the extra instances
# from being placed.
separate_scope    = "$${node.datacenter}"
separate_required = false

# At most one instance down during drains and evictions.
disruption_max_unavailable = "1"

# Discovery prefers instances in the datacenter of the caller. With one node
# per datacenter a datacenter usually holds one or two instances:
# min_local = 2 adds the instances of the rest of the region whenever the
# caller's datacenter has fewer than two healthy ones, so one failing
# instance never takes all of the datacenter's traffic.
discovery_prefer    = "datacenter"
discovery_min_local = 2

scaling_enabled    = true
scaling_min        = 3
scaling_max        = 6
scaling_cpu_target = 70
