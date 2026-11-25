# The variables of the cache. See ../webapp/variables.hcl for what a
# variable block takes.

variable "image" {
  type        = string
  default     = "valkey/valkey:8.0-alpine"
  description = "Container image of Valkey."

  validation {
    min_length = 1
  }

  ui {
    tab = "general"
  }
}

variable "port" {
  type        = number
  default     = 6379
  description = "Port Valkey listens on inside its network namespace."

  validation {
    min     = 1
    max     = 65535
    integer = true
  }

  ui {
    tab = "general"
  }
}

variable "memory" {
  type        = number
  default     = 256
  description = "Memory of the server in MiB; Valkey evicts keys at 80 % of it."

  validation {
    min     = 64
    max     = 65536
    integer = true
  }

  ui {
    tab     = "general"
    unit    = "MiB"
    presets = [128, 256, 512, 1024]
  }
}

variable "eviction" {
  type        = string
  default     = "allkeys-lru"
  description = "What Valkey evicts when the memory is full (maxmemory-policy)."

  validation {
    one_of = ["allkeys-lru", "allkeys-lfu", "volatile-lru", "noeviction"]
  }

  ui {
    tab = "general"
  }
}

# The password is in the secret store; the server reads it when it starts.
variable "password_secret" {
  type        = string
  default     = "cache/password"
  description = "Path of the secret with the item password, which the server requires of every client."

  validation {
    pattern = "^[a-z0-9_-]+(/[a-z0-9_-]+)*$"
  }

  secret {
    path = "cache/password"
    item = "password"
  }

  ui {
    tab    = "general"
    widget = "secret-path"
  }
}

variable "persistence" {
  type        = bool
  default     = false
  description = "Keep the data on a host volume (append-only file). Without it the cache starts empty."

  ui {
    tab    = "storage"
    widget = "switch"
  }
}

variable "host_volume" {
  type        = string
  default     = "cache"
  description = "Host volume of the data, mounted at /data; with persistence. The server runs only on nodes that have it."

  validation {
    min_length = 1
  }

  ui {
    tab     = "storage"
    show_if = "persistence"
  }
}
