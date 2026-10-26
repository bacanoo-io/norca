# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# A client of the test-api service over native mTLS: its Envoy sidecar
# exposes the upstream test-api on 127.0.0.1:9001 inside the group network,
# and the service intentions of the namespace decide whether the connection
# is allowed. The probe fetches the page every 10 seconds; see its log with
#   norca alloc logs -namespace apps -task probe <alloc ID>
job "probe" {
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

    # Cross-job affinity: prefer a node that runs an instance of test-app
    # (a preference, not a rule; scope: the node).
    colocate {
      selector {
        app = "test-app"
      }
      scope  = "${node.unique.id}"
      weight = 50
    }

    # The certificate of the sidecar carries the DNS name of this service,
    # which the intentions of the destination refer to (source "probe").
    service {
      name     = "probe"
      provider = "nomad"
      port     = "8080" # nothing listens there: the probe only connects out

      mtls {
        mode = "sidecar"

        upstream "test-api" {
          service    = "test-api"
          local_port = 9001
        }
      }
    }

    task "probe" {
      driver = "docker"

      config {
        # busybox (GPL-2.0), referenced by tag; pin a digest in production.
        image   = "busybox:1.37.0"
        command = "/bin/sh"
        args    = ["-c", "while true; do wget -q -T 5 -O - http://127.0.0.1:9001/ | grep -i '<h1>' || echo 'test-api: no connection'; sleep 10; done"]
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
