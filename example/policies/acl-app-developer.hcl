# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ACL policy of the developers of the "apps" namespace:
#   norca acl policy apply -description "Developers of apps" app-developer acl-app-developer.hcl
#   norca acl token create -name dev -policy app-developer
namespace "apps" {
  # Jobs, allocations, logs, exec, scaling (the "write" shorthand), plus the
  # Norca capabilities, which no shorthand includes.
  policy = "write"
  capabilities = [
    "list-secrets",
    "write-intention",
    "register-external-service",
  ]

  # Secrets of the built-in store below every job name. read-secret is
  # needed to submit jobs whose secret_access binds a path (the submitter
  # must be allowed to read what the job grants to its tasks).
  secret "*" {
    capabilities = ["list-secrets", "read-secret", "write-secret"]
  }

  # Network policies of the namespace.
  network_policy {
    policy = "write"
  }

  # Pack releases (packs.norca.io/Release objects) in the namespace.
  object "packs.norca.io/Release" {
    capabilities = ["read", "write"]
  }
}

# Read the quota of the namespace.
quota {
  policy = "read"
}

node {
  policy = "read"
}
