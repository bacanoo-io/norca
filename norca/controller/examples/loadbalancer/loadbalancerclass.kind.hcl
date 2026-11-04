# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The LoadBalancerClass object kind: the registry of load balancer providers,
# for running the controller with -objects=native. Register it (operator
# write is required) with:
#
#   norca object-kind apply loadbalancerclass.kind.hcl
#
# A provider is an external application that implements the provider API
# (see "Provider API" in the README of the operator). When it starts, it
# registers the class it handles, for example:
#
#   kind = "norca.io/LoadBalancerClass"
#   name = "hcloud-lb"
#   spec {
#     endpoint      = "https://lb-hetzner.service.eu.norca:8443"
#     tokenVariable = "lb-provider/hcloud/token"
#     capabilities {
#       internal = true
#       ports    = "1-65535"
#     }
#     parameters {
#       location = "fsn1"
#     }
#   }
#
# and reports a heartbeat in the status (status.lastHeartbeat, RFC 3339) while
# it runs. A load balancer whose class has no registration is reported as
# UnknownClass, and one whose provider stopped reporting for longer than
# -class-ttl as ProviderUnavailable; both are checked again by themselves.
kind "LoadBalancerClass" {
  group       = "norca.io"
  description = "A load balancer provider: the class it handles and where the operator calls it"
  namespaced  = false
  schema      = file("loadbalancerclass.schema.json")

  columns {
    column "Endpoint" { path = "spec.endpoint" }
    column "Internal" { path = "spec.capabilities.internal" }
    column "Ports" { path = "spec.capabilities.ports" }
    column "Last seen" { path = "status.lastHeartbeat" }
  }
}
