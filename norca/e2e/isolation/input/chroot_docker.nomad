# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

job "chroot_docker" {
  type = "batch"

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "print" {
    task "env" {
      driver = "docker"
      config {
        image = "busybox:1"
        args = [
          "/bin/sh",
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
