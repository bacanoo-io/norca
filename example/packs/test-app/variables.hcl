# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ---------------------------------------------------------------------------
# Placement
# ---------------------------------------------------------------------------

variable "job_name" {
  description = "Name of the job and of its service in the catalog. The secrets of the job are read from <job_name>/db."
  type        = string
  default     = "test-app"
}

variable "namespace" {
  description = "Namespace of the job. Empty leaves it to the namespace of the release (--namespace) or of the submitter."
  type        = string
  default     = ""
}

variable "region" {
  description = "Region of the job. Empty leaves it to the region of the release. Must be empty with multiregion."
  type        = string
  default     = ""
}

variable "datacenters" {
  description = "Datacenters eligible for placement."
  type        = list(string)
  default     = ["*"]
}

variable "count" {
  description = "Number of instances. With scaling_enabled it must be between scaling_min and scaling_max."
  type        = number
  default     = 2
}

variable "team" {
  description = "Value of the job meta \"team\" (the admission policy of the examples warns about jobs without it)."
  type        = string
  default     = "demo"
}

# ---------------------------------------------------------------------------
# Task
# ---------------------------------------------------------------------------

variable "image" {
  description = "Container image. The official nginx image (BSD-2-Clause nginx; the image also contains Alpine packages under their own licenses), pinned by tag. Pin a digest in production."
  type        = string
  default     = "nginx:1.28.0-alpine"
}

variable "network_mode" {
  description = "Group network mode: \"bridge\" (needs the CNI plugins and a root client; required for mtls_sidecar and for network policies) or \"host\" (Docker port mapping, for dev agents without CNI)."
  type        = string
  default     = "bridge"
}

variable "cpu" {
  description = "CPU of the task, in MHz."
  type        = number
  default     = 100
}

variable "memory" {
  description = "Memory of the task, in MB."
  type        = number
  default     = 64
}

# ---------------------------------------------------------------------------
# Secrets (built-in secret store)
# ---------------------------------------------------------------------------

variable "secrets_enabled" {
  description = "Deliver the secret <job_name>/db (items username and password) with a secret block (file and env) and read its version with a Vault-style template. The task waits until the secret exists."
  type        = bool
  default     = true
}

# ---------------------------------------------------------------------------
# Health checks, disruption budget, placement rules
# ---------------------------------------------------------------------------

variable "startup_timeout" {
  description = "startup_timeout of the startup check: the task is restarted if it does not pass within this time."
  type        = string
  default     = "2m"
}

variable "disruption_max_unavailable" {
  description = "max_unavailable of the disruption block (count or percentage). Empty renders no disruption block."
  type        = string
  default     = "1"
}

variable "separate_scope" {
  description = "Scope of a separate block that keeps the instances of the job apart, e.g. \"$${node.unique.id}\" or \"$${node.datacenter}\". Empty renders no separate block."
  type        = string
  default     = ""
}

variable "separate_required" {
  description = "Make the separate block a hard rule (feasibility) instead of a preference."
  type        = bool
  default     = false
}

variable "discovery_prefer" {
  description = "prefer of the discovery block of the service: \"datacenter\", \"region\" or \"none\". Empty renders no discovery block."
  type        = string
  default     = ""
}

variable "discovery_min_local" {
  description = "min_local of the discovery block: a topology tier (the datacenter, then the rest of the region) is enough on its own only when it holds at least this many healthy instances (and at least overflow_fraction, default 20%, of all). 0 renders no min_local (the default of 1 applies)."
  type        = number
  default     = 0
}

variable "discovery_failover" {
  description = "Regions queried, in order, when the local region has no healthy instance."
  type        = list(string)
  default     = []
}

# ---------------------------------------------------------------------------
# Built-in autoscaler
# ---------------------------------------------------------------------------

variable "scaling_enabled" {
  description = "Render a scaling block with a typed policy (engine builtin, nomad.cpu, target-value). Needs server { autoscaler { enabled = true } }."
  type        = bool
  default     = true
}

variable "scaling_min" {
  description = "Minimum count of the scaling policy."
  type        = number
  default     = 1
}

variable "scaling_max" {
  description = "Maximum count of the scaling policy."
  type        = number
  default     = 4
}

variable "scaling_cpu_target" {
  description = "Target CPU usage (percent of the reserved CPU) of the target-value strategy."
  type        = number
  default     = 70
}

# ---------------------------------------------------------------------------
# Native mTLS
# ---------------------------------------------------------------------------

variable "mtls_sidecar" {
  description = "Put the service behind an Envoy sidecar (mtls { mode = \"sidecar\" }): reachable only with mTLS and allowed service intentions. Needs network_mode = \"bridge\" and Docker on the clients."
  type        = bool
  default     = false
}

# ---------------------------------------------------------------------------
# Multiregion deployment
# ---------------------------------------------------------------------------

variable "multiregion" {
  description = "Regions of a multiregion deployment, in rollout order, e.g. [{name = \"eu-1\", count = 2}, {name = \"eu-2\", count = 1}]. Empty renders no multiregion block. With regions, the group count is 0 so that every region uses its own count."
  type = list(object({
    name  = string
    count = number
  }))
  default = []
}

variable "multiregion_max_parallel" {
  description = "Number of regions that deploy at the same time."
  type        = number
  default     = 1
}

variable "multiregion_on_failure" {
  description = "on_failure of the multiregion strategy: \"fail_all\" or \"fail_local\"."
  type        = string
  default     = "fail_all"
}
