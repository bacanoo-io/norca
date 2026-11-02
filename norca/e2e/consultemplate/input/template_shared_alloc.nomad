# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

job "template-shared-alloc" {
  datacenters = ["dc1", "dc2"]

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "template-paths" {

    task "raw_exec" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 300"]
      }

      lifecycle {
        hook    = "prestart"
        sidecar = true
      }

      artifact {
        source      = "https://google.com"
        destination = "../alloc/google1.html"
      }

      template {
        destination = "${NORCA_ALLOC_DIR}/${NORCA_TASK_NAME}.env"
        data        = <<EOH
{{env "NORCA_ALLOC_DIR"}}
EOH
      }

      template {
        destination = "${NORCA_ALLOC_DIR}/hello-from-raw.env"
        data        = <<EOH
HELLO_FROM={{env "NORCA_TASK_NAME"}}
EOH
        env         = true
      }

      resources {
        cpu    = 128
        memory = 64
      }

    }

    task "docker" {
      driver = "docker"
      config {
        image   = "busybox:1"
        command = "/bin/sh"
        args    = ["-c", "sleep 300"]
      }

      artifact {
        source      = "https://google.com"
        destination = "../alloc/google2.html"
      }

      template {
        destination = "${NORCA_ALLOC_DIR}/${NORCA_TASK_NAME}.env"
        data        = <<EOH
{{env "NORCA_ALLOC_DIR"}}
EOH
      }

      template {
        source      = "${NORCA_ALLOC_DIR}/hello-from-raw.env"
        destination = "${NORCA_LOCAL_DIR}/hello-from-raw.env"
        env         = true
      }

      resources {
        cpu    = 128
        memory = 64
      }
    }

    task "exec" {
      driver = "exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 300"]
      }

      artifact {
        source      = "https://google.com"
        destination = "${NORCA_ALLOC_DIR}/google3.html"
      }

      template {
        destination = "${NORCA_ALLOC_DIR}/${NORCA_TASK_NAME}.env"
        data        = <<EOH
{{env "NORCA_ALLOC_DIR"}}
EOH
      }

      template {
        source      = "${NORCA_ALLOC_DIR}/hello-from-raw.env"
        destination = "${NORCA_LOCAL_DIR}/hello-from-raw.env"
        env         = true
      }

      resources {
        cpu    = 128
        memory = 64
      }
    }

  }
}
