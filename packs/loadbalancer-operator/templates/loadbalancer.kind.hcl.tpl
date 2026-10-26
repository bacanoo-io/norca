[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
kind "LoadBalancer" {
  group       = "norca.io"
  description = "Load balancer forwarding to the healthy instances of a Nomad service"
  namespaced  = true
  version     = 1
  schema      = <<EOT
[[ template "lb.schema" . ]]
EOT

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
  [[- if $v.webhook_enabled ]]

  webhook "default" {
    mode = "default"
    url  = [[ printf "%s/default" (trimSuffix "/" $v.webhook_url) | quote ]]
    [[- if ne $v.webhook_ca_cert "" ]]
    ca_cert = <<EOT
[[ $v.webhook_ca_cert | trim ]]
EOT
    [[- end ]]
    failure_policy = [[ $v.webhook_failure_policy | quote ]]
  }

  webhook "validate" {
    mode = "validate"
    url  = [[ printf "%s/validate" (trimSuffix "/" $v.webhook_url) | quote ]]
    [[- if ne $v.webhook_ca_cert "" ]]
    ca_cert = <<EOT
[[ $v.webhook_ca_cert | trim ]]
EOT
    [[- end ]]
    failure_policy = [[ $v.webhook_failure_policy | quote ]]
  }
  [[- end ]]

  limits {
    write_rate                = [[ $v.write_rate ]]
    identity_write_rate       = [[ $v.identity_write_rate ]]
    max_objects_per_namespace = [[ $v.max_objects_per_namespace ]]
    max_watches_per_identity  = [[ $v.max_watches_per_identity ]]
  }
}
