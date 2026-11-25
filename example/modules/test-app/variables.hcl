# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The variables of the module. The namespace and the region are those of
# the release (rigger.namespace, rigger.region). The ui blocks lay out the
# install form (tabs in metadata.hcl); the validation and rule blocks are
# checked before anything is evaluated.

# ---------------------------------------------------------------------------
# Identity and placement
# ---------------------------------------------------------------------------

variable "job_name" {
  type        = string
  default     = "test-app"
  description = "Name of the job and of its service in the catalog. The secrets of the job are read from <job_name>/db."

  validation {
    pattern = "^[a-z0-9][a-z0-9_-]{0,62}$"
  }

  ui {
    tab   = "general"
    group = "Identity"
    order = 1
    label = "Job name"
  }
}

variable "team" {
  type        = string
  default     = "demo"
  description = "Value of the job meta \"team\"."

  ui {
    tab   = "general"
    group = "Identity"
    order = 2
  }
}

variable "datacenters" {
  type        = list(string)
  default     = ["*"]
  description = "Datacenters eligible for placement."

  validation {
    min_items = 1
    unique    = true

    item {
      min_length = 1
    }
  }

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 3
    widget = "tags"
  }
}

# ---------------------------------------------------------------------------
# Task
# ---------------------------------------------------------------------------

variable "image" {
  type        = string
  default     = "nginx:1.28.0-alpine"
  description = "Container image. The official nginx image (BSD-2-Clause nginx; the image also contains Alpine packages under their own licenses), pinned by tag. Pin a digest in production."

  validation {
    min_length = 1
  }

  ui {
    tab   = "general"
    group = "Identity"
    order = 4
    label = "Container image"
  }
}

variable "count" {
  type        = number
  default     = 2
  description = "Number of instances. With scaling_enabled it must be between scaling_min and scaling_max."

  validation {
    min     = 0
    max     = 20
    integer = true
  }

  ui {
    tab    = "resources"
    group  = "Sizing"
    order  = 1
    label  = "Instances"
    widget = "slider"
  }
}

variable "cpu" {
  type        = number
  default     = 100
  description = "CPU of the task, in MHz."

  validation {
    min     = 20
    integer = true
  }

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 2
    label   = "CPU"
    unit    = "MHz"
    presets = [100, 250, 500, 1000]
  }
}

variable "memory" {
  type        = number
  default     = 64
  description = "Memory of the task, in MB."

  validation {
    min     = 16
    integer = true
  }

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 3
    unit    = "MB"
    presets = [64, 128, 256, 512]
  }
}

variable "network_mode" {
  type        = string
  default     = "bridge"
  description = "Group network mode: \"bridge\" (needs the CNI plugins and a root client; required for mtls_sidecar and for network policies) or \"host\" (Docker port mapping, for dev agents without CNI)."

  validation {
    one_of = ["bridge", "host"]
  }

  ui {
    tab           = "networking"
    group         = "Network"
    order         = 1
    label         = "Network mode"
    widget        = "segmented"
    option_labels = { bridge = "Bridge (CNI)", host = "Host" }
  }
}

# ---------------------------------------------------------------------------
# Secrets (built-in secret store)
# ---------------------------------------------------------------------------

variable "secrets_enabled" {
  type        = bool
  default     = true
  description = "Deliver the secret <job_name>/db (items username and password) with a secret block (file and env) and read its version with a Vault-style template. The task waits until the secret exists."

  ui {
    tab    = "general"
    group  = "Secrets"
    order  = 5
    label  = "Deliver the secret <job_name>/db"
    widget = "switch"
  }
}

# ---------------------------------------------------------------------------
# Health checks, disruption budget, placement rules
# ---------------------------------------------------------------------------

variable "startup_timeout" {
  type        = string
  default     = "2m"
  description = "startup_timeout of the startup check: the task is restarted if it does not pass within this time."

  validation {
    pattern = "^[0-9]+(s|m|h)$"
  }

  ui {
    tab   = "availability"
    group = "Health checks"
    order = 1
  }
}

variable "disruption_max_unavailable" {
  type        = string
  default     = "1"
  description = "max_unavailable of the disruption block (count or percentage). Empty renders no disruption block."

  validation {
    pattern = "^([0-9]+%?)?$"
  }

  ui {
    tab   = "availability"
    group = "Disruption budget"
    order = 2
    label = "Max unavailable"
  }
}

variable "separate_scope" {
  type        = string
  default     = ""
  description = "Scope of a separate block that keeps the instances of the job apart, e.g. \"$${node.unique.id}\" or \"$${node.datacenter}\". Empty renders no separate block."

  ui {
    tab   = "availability"
    group = "Anti-affinity"
    order = 3
  }
}

variable "separate_required" {
  type        = bool
  default     = false
  description = "Make the separate block a hard rule (feasibility) instead of a preference."

  ui {
    tab     = "availability"
    group   = "Anti-affinity"
    order   = 4
    widget  = "switch"
    show_if = "separate_scope != \"\""
  }
}

variable "discovery_prefer" {
  type        = string
  default     = ""
  description = "prefer of the discovery block of the service: \"datacenter\", \"region\" or \"none\". Empty renders no discovery block."

  validation {
    one_of = ["", "none", "datacenter", "region"]
  }

  ui {
    tab           = "networking"
    group         = "Service discovery"
    order         = 2
    widget        = "select"
    option_labels = { "" = "No discovery block", none = "None", datacenter = "Datacenter", region = "Region" }
  }
}

variable "discovery_min_local" {
  type        = number
  default     = 0
  description = "min_local of the discovery block: a topology tier (the datacenter, then the rest of the region) is enough on its own only when it holds at least this many healthy instances (and at least overflow_fraction, default 20%, of all). 0 renders no min_local (the default of 1 applies)."

  validation {
    min     = 0
    integer = true
  }

  ui {
    tab     = "networking"
    group   = "Service discovery"
    order   = 3
    show_if = "discovery_prefer != \"\""
  }
}

variable "discovery_failover" {
  type        = list(string)
  default     = []
  description = "Regions queried, in order, when the local region has no healthy instance."

  validation {
    item {
      min_length = 1
    }
  }

  ui {
    tab     = "networking"
    group   = "Service discovery"
    order   = 4
    widget  = "tags"
    show_if = "discovery_prefer != \"\""
  }
}

# ---------------------------------------------------------------------------
# Built-in autoscaler
# ---------------------------------------------------------------------------

variable "scaling_enabled" {
  type        = bool
  default     = true
  description = "Render a scaling block with a typed policy (engine builtin, nomad.cpu, target-value). Needs server { autoscaler { enabled = true } }."

  ui {
    tab    = "resources"
    group  = "Autoscaling"
    order  = 4
    label  = "Built-in autoscaler"
    widget = "switch"
  }
}

variable "scaling_min" {
  type        = number
  default     = 1
  description = "Minimum count of the scaling policy."

  validation {
    min     = 0
    integer = true
  }

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 5
    show_if = "scaling_enabled"
  }
}

variable "scaling_max" {
  type        = number
  default     = 4
  description = "Maximum count of the scaling policy."

  validation {
    min     = 1
    max     = 20
    integer = true
  }

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 6
    show_if = "scaling_enabled"
  }
}

variable "scaling_cpu_target" {
  type        = number
  default     = 70
  description = "Target CPU usage (percent of the reserved CPU) of the target-value strategy."

  validation {
    min = 1
    max = 100
  }

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 7
    label   = "CPU target"
    unit    = "%"
    widget  = "slider"
    show_if = "scaling_enabled"
  }
}

# ---------------------------------------------------------------------------
# Native mTLS
# ---------------------------------------------------------------------------

variable "mtls_sidecar" {
  type        = bool
  default     = false
  description = "Put the service behind an Envoy sidecar (mtls { mode = \"sidecar\" }): reachable only with mTLS and allowed service intentions. Needs network_mode = \"bridge\" and Docker on the clients."

  ui {
    tab     = "networking"
    group   = "Network"
    order   = 5
    label   = "Native mTLS sidecar"
    widget  = "switch"
    show_if = "network_mode == \"bridge\""
  }
}

# ---------------------------------------------------------------------------
# Multiregion deployment
# ---------------------------------------------------------------------------

variable "multiregion" {
  type = list(object({
    name  = string
    count = number
  }))
  default     = []
  description = "Regions of a multiregion deployment, in rollout order, e.g. [{name = \"eu-1\", count = 2}, {name = \"eu-2\", count = 1}]. Empty renders no multiregion block. With regions, the group count is 0 so that every region uses its own count."

  ui {
    tab    = "multiregion"
    group  = "Regions"
    order  = 1
    widget = "object-list"
  }
}

variable "multiregion_max_parallel" {
  type        = number
  default     = 1
  description = "Number of regions that deploy at the same time."

  validation {
    min     = 0
    integer = true
  }

  ui {
    tab   = "multiregion"
    group = "Strategy"
    order = 2
  }
}

variable "multiregion_on_failure" {
  type        = string
  default     = "fail_all"
  description = "on_failure of the multiregion strategy: \"fail_all\" or \"fail_local\"."

  validation {
    one_of = ["fail_all", "fail_local"]
  }

  ui {
    tab    = "multiregion"
    group  = "Strategy"
    order  = 3
    widget = "segmented"
  }
}

# ---------------------------------------------------------------------------
# Rules across variables
# ---------------------------------------------------------------------------

rule "image_pinned" {
  condition     = !can(regex(":latest$", var.image))
  error_message = "image: pin a tag or a digest instead of :latest."
}

rule "scaling_bounds" {
  condition     = !var.scaling_enabled || var.scaling_min <= var.scaling_max
  error_message = "scaling_min must not be above scaling_max."
}

rule "count_within_scaling" {
  condition     = !var.scaling_enabled || length(var.multiregion) > 0 || (var.count >= var.scaling_min && var.count <= var.scaling_max)
  error_message = "count must be between scaling_min and scaling_max with scaling_enabled."
}

rule "mtls_needs_bridge" {
  condition     = !var.mtls_sidecar || var.network_mode == "bridge"
  error_message = "mtls_sidecar needs network_mode = \"bridge\"."
}

rule "multiregion_regions" {
  condition     = alltrue([for r in var.multiregion : r.name != "" && r.count >= 0 && floor(r.count) == r.count])
  error_message = "multiregion: every region needs a name and a whole count of 0 or more."
}

rule "multiregion_unique" {
  condition     = length(distinct([for r in var.multiregion : r.name])) == length(var.multiregion)
  error_message = "multiregion: a region is listed more than once."
}

rule "multiregion_no_scaling" {
  condition     = length(var.multiregion) == 0 || !var.scaling_enabled
  error_message = "scaling_enabled must be false with multiregion."
}
