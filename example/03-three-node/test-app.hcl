# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Variables of the test-app pack on the three-node cluster:
#   norca pack run ../packs/test-app --name=test-app --namespace=apps -f test-app.hcl
count = 3

# Spread the instances over the zones (node meta zone). A preference, not a
# rule: with more instances than zones (the autoscaler may go up to 6) a
# required rule would keep the extra instances from being placed.
separate_scope    = "$${meta.zone}"
separate_required = false

# At most one instance down during drains and evictions.
disruption_max_unavailable = "1"

# Discovery prefers instances in the zone of the caller.
discovery_prefer = "zone"

scaling_enabled    = true
scaling_min        = 3
scaling_max        = 6
scaling_cpu_target = 70
