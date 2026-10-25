# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The LoadBalancer object kind, for running the controller with
# -objects=native. Register it (operator write is required) with:
#
#   norca object-kind apply loadbalancer.kind.hcl
#
# and create load balancers with "norca object apply", for example:
#
#   kind = "norca.io/LoadBalancer"
#   name = "web"
#   spec {
#     service = "web"
#     port    = 8080
#   }
kind "LoadBalancer" {
  group       = "norca.io"
  description = "Load balancer forwarding to the healthy instances of a Nomad service"
  namespaced  = true
  schema      = file("loadbalancer.schema.json")

  rule {
    expression = "!has(oldSelf) || self.spec.internal == oldSelf.spec.internal"
    message    = "internal can't be changed; delete and recreate the load balancer."
  }

  columns {
    column "Service" { path = "spec.service" }
    column "Port" { path = "spec.port" }
    column "Class" { path = "status.class" }
    column "Addresses" { path = "status.addresses" }
    column "Ready" { path = "status.conditions[?type=='Ready'].status" }
  }
}
