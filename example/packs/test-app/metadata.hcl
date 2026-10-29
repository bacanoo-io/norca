# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

app {
  url = "https://nginx.org"
}

pack {
  name        = "test-app"
  description = "A small test web application (the official nginx image) that shows Norca features: secrets from the built-in store (secret block and Vault-style template reads), health check roles, disruption budget, cross-job anti-affinity, a typed scaling policy for the built-in autoscaler, the native service catalog, optional native mTLS sidecar and multiregion deployment."
  version     = "0.2.0"

  requires {
    norca = ">= 0.1.0"
  }
}

# Tabs of the install form in the web UI; the variables name theirs in
# their ui blocks.
ui {
  tab "general" {
    label = "General"
    icon  = "adjustments-horizontal"
  }
  tab "resources" {
    label = "Resources & Scaling"
    icon  = "cpu"
  }
  tab "availability" {
    label = "Availability"
    icon  = "shield-check"
  }
  tab "networking" {
    label = "Networking"
    icon  = "network"
  }
  tab "multiregion" {
    label = "Multiregion"
    icon  = "world"
  }
}
