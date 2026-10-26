# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The probe of 03-three-node in region eu-2, reading test-api of region eu-1
# through the mTLS gateway of eu-1. The upstream verifies that the server
# certificate is the one of test-api.service.apps.eu-1.nomad, signed by the
# CA of eu-1; the intentions of eu-1 decide.
#   norca job run probe-eu-2.nomad.hcl
#   norca service intention create -region eu-1 -namespace apps probe test-api
job "probe" {
  region      = "eu-2"
  namespace   = "apps"
  datacenters = ["*"]
  type        = "service"

  meta {
    app  = "probe"
    team = "demo"
  }

  group "probe" {
    count = 1

    network {
      mode = "bridge"
    }

    service {
      name     = "probe"
      provider = "nomad"
      port     = "8080" # nothing listens there: the probe only connects out

      mtls {
        mode = "sidecar"

        upstream "test-api" {
          service    = "test-api"
          namespace  = "apps"
          region     = "eu-1"         # through the gateway of eu-1
          gateway    = "mtls-gateway" # the default gateway service name
          local_port = 9001
        }
      }
    }

    task "probe" {
      driver = "docker"

      config {
        image   = "busybox:1.37.0"
        command = "/bin/sh"
        args    = ["-c", "while true; do wget -q -T 5 -O - http://127.0.0.1:9001/ | grep -i '<h1>' || echo 'test-api@eu-1: no connection'; sleep 10; done"]
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
