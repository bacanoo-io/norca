# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The native mTLS gateway of a region: forwards TLS connections from other
# regions by their SNI name, without terminating them, to the mTLS services
# of its region. mTLS stays end to end between the sidecars; the intentions
# of the destination region decide. Run it in the default namespace of every
# region that offers services to other regions:
#   norca job run -var region=eu-1 mtls-gateway.nomad.hcl
#   norca job run -var region=eu-2 mtls-gateway.nomad.hcl
variable "region" {
  type = string
}

job "mtls-gateway" {
  region      = var.region
  namespace   = "default"
  datacenters = ["*"]
  type        = "system"

  group "gateway" {
    # The gateway port is static: the nodes of the other regions connect to
    # <node address>:8443 over the VPN.
    network {
      mode = "bridge"

      port "mtls" {
        static = 8443
        to     = 8443
      }
    }

    # "mtls-gateway" is the name upstreams with a region look for by
    # default. The servers inject the Envoy task mtls-gateway-mtls-gateway.
    service {
      name     = "mtls-gateway"
      port     = "mtls"
      provider = "nomad"

      mtls {
        mode = "gateway"
      }
    }
  }
}
