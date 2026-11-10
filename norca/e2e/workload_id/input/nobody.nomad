# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

job "nobodyid" {
  datacenters = ["dc1"]
  type        = "batch"

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "nobodyid" {

    # nobody task should have a file owned by nobody with -rw------- perms
    task "nobody" {
      user = "nobody"

      identity {
        file = true
      }

      driver = "docker"

      config {
        image = "bash:5"
        args  = ["-c", "stat -c 'perms=%#a username=%U' secrets/norca_token; echo done"]
      }
      resources {
        cpu    = 16
        memory = 32
        disk   = 64
      }
    }
  }
}
