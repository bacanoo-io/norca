# The variables of the module. The namespace and the region are the
# release's (rigger.namespace, rigger.region; design 6.4). The options of a
# choice are a validation, so a value outside them is refused; the form takes
# them from there and keeps only their labels.

variable "job_name" {
  type        = string
  default     = "redis"
  description = "Name of the Redis job and of its service. The Variable with the password is nomad/jobs/<job_name>."

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
  description = "Container image of Redis. The module expects the entrypoint of the official image: started as root, it makes the user redis the owner of /data and runs the server as that user."

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

variable "uid" {
  type        = number
  default     = 999
  description = "User id the server runs as in the image (999 is the user redis of the official images). The configuration file with the password belongs to it and nobody else reads it."

  validation {
    min     = 0
    max     = 65535
    integer = true
  }

  ui {
    tab   = "advanced"
    group = "Image"
    order = 2
    label = "User ID of Redis"
  }
}

variable "cpu" {
  type        = number
  default     = 500
  description = "CPU of the server in MHz."

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
  description = "Memory of the server in MB."

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
  description = "Keep the data: the append-only file (AOF) and the snapshots are written to the host volume. Without it Redis is a cache that starts empty and writes nothing to disk."

  ui {
    tab    = "resources"
    group  = "Storage"
    order  = 4
    label  = "Keep the Data (AOF)"
    widget = "switch"
  }
}

variable "host_volume" {
  type        = string
  default     = "redis"
  description = "Client host volume with the data, mounted at /data; used with persistence_enabled. The server runs only on nodes that have it."

  validation {
    min_length = 1
  }

  ui {
    tab     = "resources"
    group   = "Storage"
    order   = 5
    label   = "Host Volume"
    show_if = { field = "persistence_enabled", equals = true }
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
  description = "Port Redis listens on in the network namespace of the group (bridge). The node maps a port of its own to it; with network_mode = \"host\" Redis listens on the port the node gives. Clients find the address and the port in the service catalog."

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

variable "service_tags" {
  type        = list(string)
  default     = ["cache", "redis"]
  description = "Tags of the service in the catalog."

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
  description = "Require a password: Redis refuses every command of a client that has not sent it (AUTH)."

  ui {
    tab    = "security"
    group  = "Authentication"
    order  = 1
    label  = "Enable Password Authentication"
    widget = "switch"
  }
}

# A sensitive value that the Variable resource (auth.var.hcl) takes: Rigger
# sends it to the release API as a secret item, never in the revision
# (design 6.3). The job reads it from the Variable when it starts.
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

rule "auth_needs_password" {
  condition     = !var.auth_enabled || var.redis_password != ""
  error_message = "auth_enabled = true needs redis_password: with an empty password Redis accepts every client."
}
