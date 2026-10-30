# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# The ui blocks lay out the install form of the web UI (tabs in
# metadata.hcl); the validation and rule blocks are checked by the CLI and
# the servers before rendering.

# ---------------------------------------------------------------------------
# Placement
# ---------------------------------------------------------------------------

variable "job_name" {
  description = "Name of the job and of its service in the catalog. The secrets of the job are read from <job_name>/db."
  type        = string
  default     = "test-app"

  ui {
    tab   = "general"
    group = "Identity"
    order = 1
    label = "Job name"
  }

  validation {
    pattern = "^[a-z0-9][a-z0-9_-]{0,62}$"
  }
}

variable "team" {
  description = "Value of the job meta \"team\" (the admission policy of the examples warns about jobs without it)."
  type        = string
  default     = "demo"

  ui {
    tab   = "general"
    group = "Identity"
    order = 2
  }
}

variable "namespace" {
  description = "Namespace of the job. Empty leaves it to the namespace of the release (--namespace) or of the submitter."
  type        = string
  default     = ""

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 3
    widget = "namespace-select"
  }
}

variable "region" {
  description = "Region of the job. Empty leaves it to the region of the release. Must be empty with multiregion."
  type        = string
  default     = ""

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 4
    widget = "region-select"
  }
}

variable "datacenters" {
  description = "Datacenters eligible for placement."
  type        = list(string)
  default     = ["*"]

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 5
    widget = "tags"
  }

  validation {
    min_items       = 1
    item_min_length = 1
    unique          = true
  }
}

# ---------------------------------------------------------------------------
# Task
# ---------------------------------------------------------------------------

variable "image" {
  description = "Container image. The official nginx image (BSD-2-Clause nginx; the image also contains Alpine packages under their own licenses), pinned by tag. Pin a digest in production."
  type        = string
  default     = "nginx:1.28.0-alpine"

  ui {
    tab   = "general"
    group = "Identity"
    order = 6
    label = "Container image"
  }

  validation {
    min_length = 1
  }
}

variable "count" {
  description = "Number of instances. With scaling_enabled it must be between scaling_min and scaling_max."
  type        = number
  default     = 2

  ui {
    tab    = "resources"
    group  = "Sizing"
    order  = 1
    label  = "Instances"
    widget = "slider"
  }

  validation {
    min     = 0
    max     = 20
    integer = true
  }
}

variable "cpu" {
  description = "CPU of the task, in MHz."
  type        = number
  default     = 100

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 2
    label   = "CPU"
    unit    = "MHz"
    presets = [100, 250, 500, 1000]
  }

  validation {
    min     = 20
    integer = true
  }
}

variable "memory" {
  description = "Memory of the task, in MB."
  type        = number
  default     = 64

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 3
    unit    = "MB"
    presets = [64, 128, 256, 512]
  }

  validation {
    min     = 16
    integer = true
  }
}

variable "network_mode" {
  description = "Group network mode: \"bridge\" (needs the CNI plugins and a root client; required for mtls_sidecar and for network policies) or \"host\" (Docker port mapping, for dev agents without CNI)."
  type        = string
  default     = "bridge"

  ui {
    tab           = "networking"
    group         = "Network"
    order         = 1
    label         = "Network mode"
    widget        = "segmented"
    options       = ["bridge", "host"]
    option_labels = { bridge = "Bridge (CNI)", host = "Host" }
  }
}

# ---------------------------------------------------------------------------
# Secrets (built-in secret store)
# ---------------------------------------------------------------------------

variable "secrets_enabled" {
  description = "Deliver the secret <job_name>/db (items username and password) with a secret block (file and env) and read its version with a Vault-style template. The task waits until the secret exists."
  type        = bool
  default     = true

  ui {
    tab    = "general"
    group  = "Secrets"
    order  = 7
    label  = "Deliver the secret <job_name>/db"
    widget = "switch"
  }
}

# ---------------------------------------------------------------------------
# Health checks, disruption budget, placement rules
# ---------------------------------------------------------------------------

variable "startup_timeout" {
  description = "startup_timeout of the startup check: the task is restarted if it does not pass within this time."
  type        = string
  default     = "2m"

  ui {
    tab   = "availability"
    group = "Health checks"
    order = 1
  }

  validation {
    pattern = "^[0-9]+(s|m|h)$"
  }
}

variable "disruption_max_unavailable" {
  description = "max_unavailable of the disruption block (count or percentage). Empty renders no disruption block."
  type        = string
  default     = "1"

  ui {
    tab   = "availability"
    group = "Disruption budget"
    order = 2
    label = "Max unavailable"
  }

  validation {
    pattern = "^([0-9]+%?)?$"
  }
}

variable "separate_scope" {
  description = "Scope of a separate block that keeps the instances of the job apart, e.g. \"$${node.unique.id}\" or \"$${node.datacenter}\". Empty renders no separate block."
  type        = string
  default     = ""

  ui {
    tab   = "availability"
    group = "Anti-affinity"
    order = 3
  }
}

variable "separate_required" {
  description = "Make the separate block a hard rule (feasibility) instead of a preference."
  type        = bool
  default     = false

  ui {
    tab     = "availability"
    group   = "Anti-affinity"
    order   = 4
    widget  = "switch"
    show_if = { field = "separate_scope", notEquals = "" }
  }
}

variable "discovery_prefer" {
  description = "prefer of the discovery block of the service: \"datacenter\", \"region\" or \"none\". Empty renders no discovery block."
  type        = string
  default     = ""

  ui {
    tab           = "networking"
    group         = "Service discovery"
    order         = 2
    widget        = "select"
    options       = ["", "none", "datacenter", "region"]
    option_labels = { "" = "No discovery block", none = "None", datacenter = "Datacenter", region = "Region" }
  }
}

variable "discovery_min_local" {
  description = "min_local of the discovery block: a topology tier (the datacenter, then the rest of the region) is enough on its own only when it holds at least this many healthy instances (and at least overflow_fraction, default 20%, of all). 0 renders no min_local (the default of 1 applies)."
  type        = number
  default     = 0

  ui {
    tab     = "networking"
    group   = "Service discovery"
    order   = 3
    show_if = { field = "discovery_prefer", in = ["datacenter", "region"] }
  }

  validation {
    min     = 0
    integer = true
  }
}

variable "discovery_failover" {
  description = "Regions queried, in order, when the local region has no healthy instance."
  type        = list(string)
  default     = []

  ui {
    tab     = "networking"
    group   = "Service discovery"
    order   = 4
    widget  = "tags"
    show_if = { field = "discovery_prefer", notEquals = "" }
  }

  validation {
    item_min_length = 1
  }
}

# ---------------------------------------------------------------------------
# Built-in autoscaler
# ---------------------------------------------------------------------------

variable "scaling_enabled" {
  description = "Render a scaling block with a typed policy (engine builtin, nomad.cpu, target-value). Needs server { autoscaler { enabled = true } }."
  type        = bool
  default     = true

  ui {
    tab    = "resources"
    group  = "Autoscaling"
    order  = 4
    label  = "Built-in autoscaler"
    widget = "switch"
  }
}

variable "scaling_min" {
  description = "Minimum count of the scaling policy."
  type        = number
  default     = 1

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 5
    show_if = "scaling_enabled"
  }

  validation {
    min     = 0
    integer = true
  }
}

variable "scaling_max" {
  description = "Maximum count of the scaling policy."
  type        = number
  default     = 4

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 6
    show_if = "scaling_enabled"
  }

  validation {
    min     = 1
    max     = 20
    integer = true
  }
}

variable "scaling_cpu_target" {
  description = "Target CPU usage (percent of the reserved CPU) of the target-value strategy."
  type        = number
  default     = 70

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 7
    label   = "CPU target"
    unit    = "%"
    widget  = "slider"
    show_if = "scaling_enabled"
  }

  validation {
    min = 1
    max = 100
  }
}

# ---------------------------------------------------------------------------
# Native mTLS
# ---------------------------------------------------------------------------

variable "mtls_sidecar" {
  description = "Put the service behind an Envoy sidecar (mtls { mode = \"sidecar\" }): reachable only with mTLS and allowed service intentions. Needs network_mode = \"bridge\" and Docker on the clients."
  type        = bool
  default     = false

  ui {
    tab     = "networking"
    group   = "Network"
    order   = 5
    label   = "Native mTLS sidecar"
    widget  = "switch"
    show_if = { field = "network_mode", equals = "bridge" }
  }
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

  ui {
    tab    = "multiregion"
    group  = "Regions"
    order  = 1
    widget = "object-list"
  }
}

variable "multiregion_max_parallel" {
  description = "Number of regions that deploy at the same time."
  type        = number
  default     = 1

  ui {
    tab   = "multiregion"
    group = "Strategy"
    order = 2
  }

  validation {
    min     = 0
    integer = true
  }
}

variable "multiregion_on_failure" {
  description = "on_failure of the multiregion strategy: \"fail_all\" or \"fail_local\"."
  type        = string
  default     = "fail_all"

  ui {
    tab     = "multiregion"
    group   = "Strategy"
    order   = 3
    widget  = "segmented"
    options = ["fail_all", "fail_local"]
  }
}

# ---------------------------------------------------------------------------
# Rules across variables
# ---------------------------------------------------------------------------

rule "image_pinned" {
  condition     = !can(regex(":latest$", var.image))
  error_message = "image: pin a tag or a digest instead of :latest (${var.image})."
}

rule "scaling_bounds" {
  condition     = !var.scaling_enabled || var.scaling_min <= var.scaling_max
  error_message = "scaling_min (${var.scaling_min}) must not be above scaling_max (${var.scaling_max})."
}

rule "count_within_scaling" {
  condition     = !var.scaling_enabled || length(var.multiregion) > 0 || (var.count >= var.scaling_min && var.count <= var.scaling_max)
  error_message = "count (${var.count}) must be between scaling_min (${var.scaling_min}) and scaling_max (${var.scaling_max}) with scaling_enabled."
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

rule "multiregion_no_region" {
  condition     = length(var.multiregion) == 0 || var.region == ""
  error_message = "region must be empty with multiregion: every region of the multiregion block gets the job."
}

rule "multiregion_no_scaling" {
  condition     = length(var.multiregion) == 0 || !var.scaling_enabled
  error_message = "scaling_enabled must be false with multiregion."
}
