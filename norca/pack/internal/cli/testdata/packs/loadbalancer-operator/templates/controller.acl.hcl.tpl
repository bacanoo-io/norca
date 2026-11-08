[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
[[- if $v.acl_policy_enabled ]]
policy [[ printf "%s" $v.job_name | quote ]] {
  description = "Permissions of the LoadBalancer operator (bound to its controller job)"
  rules       = <<EOT
namespace "[[ $v.namespace ]]" {
  # Read services, allocations and jobs; register and purge the proxy jobs
  # (lb-*) the operator owns.
  capabilities = ["list-jobs", "read-job", "submit-job"]

  variables {
    # Leader election leases of the controller instances.
    path "controller-lease/loadbalancer*" {
      capabilities = ["list", "read", "write"]
    }

    # The target lists of the proxy jobs (the jobs' own Variables).
    path "nomad/jobs/lb-*" {
      capabilities = ["list", "read", "write", "destroy"]
    }
[[- if eq $v.class_token_namespace $v.namespace ]]

    # The tokens of the registered classes.
    path [[ $v.class_token_path | quote ]] {
      capabilities = ["read"]
    }
[[- end ]]
  }

  # The LoadBalancer objects: status, event log and the cleanup finalizer;
  # "write" for the objects of marked services.
  object "norca.io/LoadBalancer" {
    capabilities = ["read", "write", "write-status", "write-events", "manage-finalizers"]
  }
}

[[- if ne $v.class_token_namespace $v.namespace ]]

# The tokens of the registered classes.
namespace "[[ $v.class_token_namespace ]]" {
  variables {
    path [[ $v.class_token_path | quote ]] {
      capabilities = ["read"]
    }
  }
}
[[- end ]]

# The registered load balancer classes are cluster-scoped: their rule is in
# the namespace "*".
namespace "*" {
  object "norca.io/LoadBalancerClass" {
    capabilities = ["read"]
  }
}

# The nodes of the instances: an instance on a node marked as leaving
# (norca.lb.leaving, set by the node scaling operator before a drain) is not
# a target; node events reconcile the load balancers.
node {
  policy = "read"
}
EOT

  job_acl {
    namespace = [[ $v.namespace | quote ]]
    job_id    = [[ $v.job_name | quote ]]
  }
}
[[- end ]]
