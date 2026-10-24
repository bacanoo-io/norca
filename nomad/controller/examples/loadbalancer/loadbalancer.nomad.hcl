# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The LoadBalancer operator, one per region.
#
#   1. Build the binary for the nodes and install it on them (the proxy jobs
#      the operator creates run the same binary with raw_exec):
#        cd nomad/controller && go build -o /usr/local/bin/norca-loadbalancer ./examples/loadbalancer
#   2. Bind the ACL policy to this job (loadbalancer-policy.hcl).
#   3. norca job run loadbalancer.nomad.hcl
#   4. Create a load balancer:
#        norca var put crd/norca-io/LoadBalancer/web spec='{"service":"web","port":8080}'
#        norca var get -item=status crd/norca-io/LoadBalancer/web
#      or mark a service with the norca_lb_class / norca_lb_port meta keys.
#      With native objects (-var objects=native, after
#      "norca object-kind apply loadbalancer.kind.hcl"):
#        norca object apply web.hcl   # kind = "norca.io/LoadBalancer" ...
#        norca object list norca.io/LoadBalancer
#
# Two instances run; leader election over a Variables lease makes one of
# them reconcile while the other keeps a warm cache.
#
# The controller reaches the API on the task API socket with its workload
# identity (NORCA_TOKEN falls back to the NOMAD_TOKEN set by
# identity { env = true }). In this Nomad version the event stream accepts
# only ACL tokens, so with ACLs enabled the controller falls back to
# blocking queries. To use the event stream, store an ACL token of the
# policy in the job's Variable (nomad/jobs/loadbalancer-controller, item
# "token") and enable the template below.

variable "binary" {
  type    = string
  default = "/usr/local/bin/norca-loadbalancer"
}

variable "namespace" {
  type    = string
  default = "default"
}

# Storage of the LoadBalancer objects: "variables", or "native" after
# registering the kind with "norca object-kind apply loadbalancer.kind.hcl".
variable "objects" {
  type    = string
  default = "variables"
}

job "loadbalancer-controller" {
  type = "service"

  group "controller" {
    count = 2

    network {
      port "metrics" {}
    }

    task "controller" {
      driver = "raw_exec"

      config {
        command = var.binary
        args = [
          "controller",
          "-namespace", var.namespace,
          "-objects", var.objects,
          "-proxy-driver", "raw_exec",
          "-proxy-command", var.binary,
          "-metrics-addr", "${NOMAD_ADDR_metrics}",
        ]
      }

      identity {
        env = true
      }

      env {
        NORCA_ADDR = "unix://${NOMAD_SECRETS_DIR}/api.sock"
      }

      # Optional: an ACL token for the event stream (see above).
      # template {
      #   data        = "{{ with nomadVar \"nomad/jobs/loadbalancer-controller\" }}NORCA_TOKEN={{ .token }}{{ end }}"
      #   destination = "secrets/token.env"
      #   env         = true
      # }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }
}
