# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ACL policy of the LoadBalancer operator. Bind it to the controller job, so
# the controller runs with its workload identity and needs no static token:
#
#   norca acl policy apply -namespace default -job loadbalancer-controller \
#     loadbalancer-controller loadbalancer-policy.hcl
#
# For an operator watching several namespaces (-namespace "*"), repeat the
# namespace block for each of them (or use namespace "*").

namespace "default" {
  # Read services, allocations and jobs; register and purge the proxy jobs
  # (lb-*) the operator owns.
  capabilities = ["list-jobs", "read-job", "submit-job"]

  variables {
    # The LoadBalancer objects: spec written by users, status by the
    # controller, objects of marked services created and deleted by it.
    path "crd/norca-io/LoadBalancer/*" {
      capabilities = ["list", "read", "write", "destroy"]
    }

    # Leader election leases of the two controllers of the operator.
    path "controller-lease/loadbalancer*" {
      capabilities = ["list", "read", "write"]
    }

    # The target lists of the proxy jobs (the jobs' own Variables).
    path "nomad/jobs/lb-*" {
      capabilities = ["list", "read", "write", "destroy"]
    }
  }

  # With -objects=native: the native LoadBalancer objects. "write" is
  # needed only for the objects created from marked services (discovery);
  # the status is written with "write-status", the event log with
  # "write-events", and the cleanup finalizer needs "manage-finalizers".
  # The crd/... Variables path above is then unused.
  object "norca.io/LoadBalancer" {
    capabilities = ["read", "write", "write-status", "write-events", "manage-finalizers"]
  }
}
