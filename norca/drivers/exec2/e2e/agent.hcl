# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# intended to be used with a norca agent in -dev mode (norca agent -dev -config
# agent.hcl) for the e2e tests

server {
  enabled          = true
  bootstrap_expect = 1
  default_scheduler_config {
    memory_oversubscription_enabled = true
  }
}

ui {
  show_cli_hints = false
}

client {
  enabled = true
  options = {
    "fingerprint.denylist" = "env_aws,env_gce,env_azure,env_digitalocean"
  }
}

plugin "exec2" {
  config {
    unveil_by_task = true
    unveil_paths   = ["r:/etc/mime.types"]
  }
}
