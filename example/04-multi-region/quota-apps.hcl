# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The quota of the apps namespace in the federation: one limit per region.
# Quotas and namespaces are global: apply them in the authoritative region.
#   norca quota apply -region eu-1 quota-apps.hcl
name        = "apps"
description = "Resources of the example applications, per region"

limit {
  region = "eu-1"

  region_limit {
    cpu    = 4000
    memory = 4096
  }

  allocations = 30
}

limit {
  region = "eu-2"

  region_limit {
    cpu    = 2000
    memory = 2048
  }

  allocations = 15
}

task_defaults {
  cpu    = 100
  memory = 128
}

task_max {
  cpu    = 1000
  memory = 1024
}

enforce = "schedule"
