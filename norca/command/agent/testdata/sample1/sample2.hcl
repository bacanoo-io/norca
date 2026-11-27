# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

"advertise" = {
  "http" = "host.example.com"
  "rpc"  = "host.example.com"
  "serf" = "host.example.com"
}

"autopilot" = {
  "cleanup_dead_servers" = true
}

"consul" = {
  "client_auto_join" = false
  "server_auto_join" = false
  "token"            = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
}

vault = {
  enabled = true
}
