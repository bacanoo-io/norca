# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

job "horizontally_scalable" {
  datacenters = ["dc1"]
  type        = "service"

  update {
    health_check = "task_states"
  }

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "horizontally_scalable" {

    count = 4

    scaling {
      min     = 5
      max     = 6
      enabled = true
      engine  = "external"

      policy {}
    }

    task "test" {
      driver = "raw_exec"

      config {
        command = "bash"
        args    = ["-c", "sleep 15000"]
      }
    }
  }
}
