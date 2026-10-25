[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]

[[- /* The JSON Schema of the LoadBalancer kind (nomad/controller/examples/loadbalancer/loadbalancer.schema.json).
With the webhooks, the defaulting webhook fills in the class of the region instead of the static default. */ -]]
[[- define "lb.schema" -]]
[[- $v := vars . -]]
{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["service", "port"],
      "additionalProperties": false,
      "properties": {
        "service":     {"type": "string", "minLength": 1},
        "class":       {"type": "string", "minLength": 1[[ if not $v.webhook_enabled ]], "default": "nomad"[[ end ]]},
        "port":        {"type": "integer", "minimum": 1, "maximum": 65535},
        "internal":    {"type": "boolean", "default": false},
        "replicas":    {"type": "integer", "minimum": 1, "maximum": 16, "default": 1},
        "datacenters": {"type": "array", "items": {"type": "string", "minLength": 1}, "default": ["*"]}
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "observedGeneration": {"type": "integer", "minimum": 0},
        "observedSpecHash":   {"type": "string"},
        "conditions": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["type", "status"],
            "properties": {
              "type":               {"type": "string"},
              "status":             {"enum": ["True", "False", "Unknown"]},
              "reason":             {"type": "string"},
              "message":            {"type": "string"},
              "lastTransitionTime": {"type": "string"}
            }
          }
        },
        "class":     {"type": "string"},
        "addresses": {"type": "array", "items": {"type": "string"}},
        "targets":   {"type": "array", "items": {"type": "string"}},
        "resource":  {"type": "string"}
      }
    }
  }
}
[[- end -]]

[[- /* The CEL check of one service's norca_lb_port mark (s is the service). */ -]]
[[- define "lb.port_mark_ok" -]]
(!has(s.Meta) || s.Meta == null || !('norca_lb_port' in s.Meta) || (s.Meta.norca_lb_port.matches('^[0-9]{1,5}$') && int(s.Meta.norca_lb_port) >= 1 && int(s.Meta.norca_lb_port) <= 65535))
[[- end -]]
