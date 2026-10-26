# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Quota of the "apps" namespace of the examples:
#   norca quota apply quota-apps.hcl
#   norca namespace apply namespace-apps.hcl     # attaches it
#   norca quota status apps
name        = "apps"
description = "Resources of the example applications"

# One limit per region. "global" is the default region name; add a limit
# block for every region of a federation (multi-region example).
limit {
  region = "global"

  region_limit {
    cpu    = 4000 # MHz
    memory = 4096 # MB
  }

  variables_limit = 50 # MB
  allocations     = 30
}

# Filled in for tasks that do not set them (LimitRange equivalent).
task_defaults {
  cpu    = 100
  memory = 128
}

# The most a single task may request.
task_max {
  cpu    = 1000
  memory = 1024
}

# "schedule": allocations are placed up to the limit, the rest waits as a
# blocked evaluation; "submit" also rejects jobs whose total demand exceeds
# the limit.
enforce = "schedule"
