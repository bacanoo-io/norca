# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

job "chroot_exec" {
  type = "batch"

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "print" {
    task "env" {
      driver = "exec"
      config {
        command = "/bin/bash"
        args = [
          "-c",
          "echo $NORCA_ALLOC_DIR; echo $NORCA_TASK_DIR; echo $NORCA_SECRETS_DIR; echo $PATH"
        ]
      }

      resources {
        cpu    = 50
        memory = 50
      }
    }
  }
}
