# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Variables of the test-app pack as a multiregion job: registered in both
# regions with the same version and rolled out eu-1 first, then eu-2.
#   norca pack run ../packs/test-app --name=test-app --namespace=apps \
#     --regions=eu-1,eu-2 -f test-app-multiregion.hcl
multiregion = [
  { name = "eu-1", count = 3 },
  { name = "eu-2", count = 2 },
]
multiregion_max_parallel = 1
multiregion_on_failure   = "fail_all"

# The group count is 0 (every region uses its own count), so the typed
# scaling policy is left out.
scaling_enabled = false

separate_scope = "$${meta.zone}"

# Discovery prefers the instances in the zone of the caller.
discovery_prefer = "zone"
