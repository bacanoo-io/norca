# The variables of the pack. namespace and region are gone: they are the
# release's (rigger.namespace, rigger.region; design 6.4). The options of a choice
# are a validation, so a value outside them is refused; the form takes them
# from there and keeps only their labels.

variable "job_name" {
  type        = string
  default     = "redis"
  description = "Name of the Redis job and service."

  validation {
    pattern = "^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$"
  }

  ui {
    tab   = "general"
    group = "Identity"
    order = 1
  }
}

variable "datacenters" {
  type        = list(string)
  default     = ["dc1"]
  description = "Datacenters eligible for placement."

  validation {
    min_items = 1
  }

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 6
    widget = "datacenter-multiselect"
  }
}

variable "node_pool" {
  type        = string
  default     = "default"
  description = "Node pool for placement."

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 7
    widget = "node-pool-select"
  }
}

variable "image" {
  type        = string
  default     = "redis:7.2-alpine"
  description = "Container image of Redis."

  validation {
    min_length = 1
  }

  ui {
    tab   = "general"
    group = "Identity"
    order = 2
    label = "Container Image"
  }
}

# Unused by the job: the pack writes it only into the Variable, so every
# topology runs as standalone (README.md).
variable "mode" {
  type        = string
  default     = "standalone"
  description = "Deployment topology mode."

  validation {
    one_of = ["standalone", "replica", "sentinel"]
  }

  ui {
    tab           = "general"
    group         = "Identity"
    order         = 3
    label         = "Topology Mode"
    widget        = "segmented"
    option_labels = { standalone = "Standalone", replica = "Primary + Replicas", sentinel = "High Availability (Sentinel)" }
  }
}

variable "count" {
  type        = number
  default     = 1
  description = "Number of Redis instances."

  validation {
    min     = 1
    max     = 16
    integer = true
  }

  ui {
    tab    = "resources"
    group  = "Sizing"
    order  = 1
    label  = "Instance Count"
    widget = "slider"
  }
}

variable "cpu" {
  type        = number
  default     = 500
  description = "CPU of an instance in MHz."

  validation {
    min     = 100
    max     = 16000
    integer = true
  }

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 2
    unit    = "MHz"
    presets = [250, 500, 1000, 2000, 4000]
  }
}

variable "memory" {
  type        = number
  default     = 256
  description = "Memory of an instance in MB."

  validation {
    min     = 64
    max     = 65536
    integer = true
  }

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 3
    unit    = "MB"
    presets = [128, 256, 512, 1024, 2048]
  }
}

variable "persistence_enabled" {
  type        = bool
  default     = false
  description = "Enable Append-Only File (AOF) persistence."

  ui {
    tab    = "resources"
    group  = "Storage"
    order  = 4
    label  = "Enable AOF Persistence"
    widget = "switch"
  }
}

variable "network_mode" {
  type        = string
  default     = "bridge"
  description = "Network mode of the task group."

  validation {
    one_of = ["bridge", "host"]
  }

  ui {
    tab           = "networking"
    group         = "Listeners"
    order         = 1
    widget        = "segmented"
    option_labels = { bridge = "Bridge (Isolated)", host = "Host" }
  }
}

variable "port" {
  type        = number
  default     = 6379
  description = "Redis TCP listen port."

  validation {
    min     = 1
    max     = 65535
    integer = true
  }

  ui {
    tab   = "networking"
    group = "Listeners"
    order = 2
    label = "Redis Port"
  }
}

# Unused by the job: the pack registers no service (README.md).
variable "service_tags" {
  type        = list(string)
  default     = ["cache", "redis"]
  description = "Service catalog tags."

  ui {
    tab    = "networking"
    group  = "Service Discovery"
    order  = 3
    widget = "tags"
  }
}

variable "auth_enabled" {
  type        = bool
  default     = false
  description = "Create a Norca Variable for Redis authentication."

  ui {
    tab    = "security"
    group  = "Authentication"
    order  = 1
    label  = "Enable Password Authentication"
    widget = "switch"
  }
}

# The pack's secret { path, item } wrote this value into the item password of
# the Variable. Here it is a sensitive value that the Variable resource
# (auth.var.hcl) takes: Rigger sends it to the release API as a secret item,
# never in the revision (design 6.3).
variable "redis_password" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Password of Redis; goes into the Norca Variable only, never into the release history."

  ui {
    tab     = "security"
    group   = "Authentication"
    order   = 2
    show_if = { field = "auth_enabled", equals = true }
  }
}

# Unused by the job: the pack passes no environment (README.md).
variable "extra_env" {
  type        = map(string)
  default     = {}
  description = "Additional environment variables of the Redis container."

  ui {
    tab    = "advanced"
    group  = "Environment"
    order  = 1
    label  = "Extra Environment Variables"
    widget = "key-value"
  }
}

rule "password_needs_auth" {
  condition     = var.redis_password == "" || var.auth_enabled
  error_message = "redis_password is written only with auth_enabled = true."
}
