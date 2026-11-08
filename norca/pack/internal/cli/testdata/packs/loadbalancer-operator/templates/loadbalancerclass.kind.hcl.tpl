[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
[[- if $v.class_kind_enabled ]]
# The registry of the load balancer providers (loadbalancer/operator/
# loadbalancerclass.kind.hcl of norca-platform). A provider, norca-provider
# for example, registers each class it serves and reports a heartbeat in
# status.heartbeat while it runs.
kind "LoadBalancerClass" {
  group       = "norca.io"
  description = "A load balancer provider: the class it handles and where the operator calls it"
  namespaced  = false
  version     = 1
  schema      = <<EOT
{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["endpoint"],
      "additionalProperties": false,
      "properties": {
        "endpoint":      {"type": "string", "pattern": "^https?://", "description": "Base URL of the provider API (https; http only with -class-allow-http)"},
        "tokenVariable": {"type": "string", "description": "Path of the Variable whose item token is the bearer token of the calls"},
        "capabilities": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "internal": {"type": "boolean", "default": false},
            "ports":    {"type": "string", "pattern": "^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$", "description": "Frontend ports the provider supports, e.g. 80,443,8000-8100; empty means any"}
          }
        },
        "parameters": {"type": "object", "description": "Provider specific settings, sent with every call"}
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "heartbeat": {"type": "string", "description": "RFC 3339 time of the last heartbeat"},
        "version":   {"type": "string"}
      }
    }
  }
}
EOT

  columns {
    column "Endpoint" { path = "spec.endpoint" }
    column "Internal" { path = "spec.capabilities.internal" }
    column "Ports" { path = "spec.capabilities.ports" }
    column "Last seen" { path = "status.heartbeat" }
  }
}
[[- end ]]
