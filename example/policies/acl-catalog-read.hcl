# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Read-only access to the service catalog, for the token of the clients' dns
# block (used by the built-in DNS, the catalog cache and the Envoy sidecars
# to look up upstreams and the gateways of other regions) and for the
# anonymous_token of the Consul-compatible API. read-job is the capability
# the service catalog checks; it also allows reading job specifications.
#   norca acl policy apply -description "Catalog read" catalog-read acl-catalog-read.hcl
#   norca acl token create -name catalog-read -policy catalog-read -global
namespace "*" {
  capabilities = ["read-job"]
}

node {
  policy = "read"
}
