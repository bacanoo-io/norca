# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

job "nomad_provider_service" {
  datacenters = ["dc1"]
  type        = "service"

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "nomad_provider_service" {

    service {
      name     = "${NORCA_NAMESPACE}-nomad-provider-service-primary"
      provider = "nomad"
      tags     = ["foo", "bar"]
    }

    service {
      name     = "${NORCA_NAMESPACE}-nomad-provider-service-secondary"
      provider = "nomad"
      tags     = ["baz", "buz"]
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
