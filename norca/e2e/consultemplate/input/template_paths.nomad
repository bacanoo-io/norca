# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

job "template-paths" {
  datacenters = ["dc1", "dc2"]

  meta {
    ARTIFACT_DEST_DIR = "local/foo/src"
  }

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "template-paths" {

    task "task" {

      driver = "docker"

      config {
        image   = "busybox:1"
        command = "/bin/sh"
        args    = ["-c", "sleep 300"]
      }

      artifact {
        source      = "https://google.com"
        destination = "${NORCA_META_ARTIFACT_DEST_DIR}"
      }

      template {
        source      = "${NORCA_TASK_DIR}/foo/src"
        destination = "${NORCA_SECRETS_DIR}/foo/dst"
      }

      template {
        destination = "${NORCA_ALLOC_DIR}/shared.txt"
        data        = <<EOH
Data shared between all task in alloc dir.
EOH
      }

      resources {
        cpu    = 128
        memory = 64
      }
    }
  }
}
