# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Variables of the test-app pack on the single node:
#   norca pack run ../packs/test-app --name=test-app --namespace=apps -f test-app.hcl
count = 2
team  = "demo"

# Bridge networking (CNI plugins in /opt/cni/bin, root agent).
network_mode = "bridge"

# Two instances, at most one of them down during a drain.
disruption_max_unavailable = "1"

scaling_enabled    = true
scaling_min        = 1
scaling_max        = 4
scaling_cpu_target = 70
