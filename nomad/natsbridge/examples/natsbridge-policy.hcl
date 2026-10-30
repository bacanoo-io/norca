# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ACL policy of the NATS bridge. Bind it to the bridge job, so the bridge
# runs with its workload identity and needs no static token:
#
#   norca acl policy apply -namespace default -job natsbridge \
#     natsbridge natsbridge-policy.hcl
#
# The topics need: Job, Deployment, Evaluation, Allocation, Service and
# Scaling the read-job capability in the namespaces of the events; Node the
# node read policy. Topics not covered here (ACL objects, node pools, "*")
# need a management token.

# Events of all namespaces (the bridge subscribes with -namespace "*").
# With -sync-pushes or -sync-regions the bridge also reads and syncs the
# cluster pack registries (cluster-scoped objects, authorized here), and
# -sync-regions publishes their events.
namespace "*" {
  capabilities = ["read-job"]

  object "packs.norca.io/Registry" {
    capabilities = ["read", "write"]
  }
}

# The namespace of the bridge job: the events of the namespace, plus the
# checkpoint and the leader election lease under the job's own Variables
# path. Workload identities may only read nomad/jobs/<job> implicitly; the
# bridge writes nomad/jobs/natsbridge/checkpoint and .../lease.
namespace "default" {
  capabilities = ["read-job"]

  variables {
    path "nomad/jobs/natsbridge/*" {
      capabilities = ["list", "read", "write"]
    }
  }
}

node {
  policy = "read"
}
