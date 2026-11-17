# ---------------------------------------------------------------------------
# General
# ---------------------------------------------------------------------------

variable "count" {
  type        = number
  default     = 3
  description = "Servers in the region. One is a single server; a cluster has an odd number, at least 3 for JetStream."

  validation {
    min     = 1
    max     = 9
    integer = true
  }

  ui {
    tab    = "general"
    widget = "slider"
  }
}

variable "image" {
  type        = string
  default     = "nats:2.15.0-alpine"
  description = "The NATS server image."
  ui { tab = "general" }
}

variable "image_digest" {
  type        = string
  default     = ""
  description = "Digest of the image; empty runs the tag."
  validation { pattern = "^(|sha256:[0-9a-f]{64})$" }
  ui { tab = "general" }
}

variable "datacenters" {
  type        = list(string)
  default     = ["*"]
  description = "Datacenters the servers may run in."
  validation { min_items = 1 }
  ui { tab = "general" }
}

variable "node_pool" {
  type        = string
  default     = ""
  description = "Node pool of the servers; empty is the default pool."
  ui {
    tab    = "general"
    widget = "node-pool-select"
  }
}

variable "resources" {
  type = object({
    cpu    = optional(number, 500)
    memory = optional(number, 512)
  })
  default     = {}
  description = "CPU (MHz) and memory (MiB) of a server."
  ui { tab = "general" }
}

# ---------------------------------------------------------------------------
# Cluster and JetStream
# ---------------------------------------------------------------------------

variable "ports" {
  type = object({
    client  = optional(number, 4222)
    cluster = optional(number, 6222)
    monitor = optional(number, 8222)
  })
  default     = {}
  description = "Static ports of the servers."

  validation {
    # inside the attributes of an object type
    attribute "client" {
      min = 1
      max = 65535
    }

    attribute "cluster" {
      min = 1
      max = 65535
    }

    attribute "monitor" {
      min = 1
      max = 65535
    }
  }

  ui { tab = "cluster" }
}

variable "jetstream" {
  type = object({
    enabled    = optional(bool, true)
    max_memory = optional(string, "256M")
    max_file   = optional(string, "1G")
    domain     = optional(string, "")
  })
  default     = {}
  description = "JetStream: durable streams on the ephemeral disk of the allocation."
  ui { tab = "cluster" }
}

# ---------------------------------------------------------------------------
# Users and TLS
# ---------------------------------------------------------------------------

variable "users" {
  type = list(object({
    name            = string
    account         = optional(string, "APP")
    publish_allow   = optional(list(string), [])
    subscribe_allow = optional(list(string), [])
  }))
  default     = []
  description = "The users of the applications, in NATS accounts. Empty: no authentication. The password of a user is the item password of the secret <users_secret_prefix>/<name>."

  validation {
    unique_by = "name"
    attribute "name" { pattern = "^[a-z][a-z0-9_-]{0,31}$" }
    attribute "account" { pattern = "^[A-Z][A-Z0-9_]{0,31}$" }
  }

  ui {
    tab    = "auth"
    widget = "object-list"
  }
}

variable "users_secret_prefix" {
  type        = string
  default     = "nats/users"
  description = "Prefix of the secrets with the passwords of the users."
  ui {
    tab     = "auth"
    widget  = "secret-path"
    show_if = "length(users) > 0"
  }
}

variable "tls_secret" {
  type        = string
  default     = ""
  description = "Path of the secret with the items ca, cert and key for the client and route ports. Empty: no TLS."
  ui {
    tab    = "auth"
    widget = "secret-path"
  }
}

# ---------------------------------------------------------------------------
# The changes of the Norca region (change data capture)
# ---------------------------------------------------------------------------

variable "norca_bus" {
  type = object({
    servers = list(string)
    secret  = optional(string, "norca/bus/leaf")
    tls     = optional(bool, true)
    account = optional(string, "")
  })
  default     = null
  description = "Attach the servers to the system bus of the Norca servers as leaf nodes and receive the changes of the region, one way. servers: their leaf node listeners (host:port, 4652). secret: the secret with the items token (an ACL token whose cdc rule has leaf = true) and ca. account: the NATS account the changes arrive in (empty: the account of the first user, or the global account)."

  validation {
    attribute "servers" {
      min_items = 1

      item {
        pattern = "^[A-Za-z0-9.-]+:[0-9]{1,5}$"
      }
    }
  }

  ui { tab = "cdc" }
}

# ---------------------------------------------------------------------------
# Rules across variables
# ---------------------------------------------------------------------------

rule "cluster_size" {
  condition     = var.count == 1 || var.count % 2 == 1
  error_message = "count must be 1 or an odd number: a NATS cluster with an even number of servers has no better quorum than one server less."
}

rule "ports_differ" {
  condition     = length(distinct(values(var.ports))) == length(values(var.ports))
  error_message = "the client, cluster and monitor ports must differ."
}

rule "bus_account_exists" {
  # || evaluates both sides, so a null norca_bus is guarded with ?: (design 4.1).
  condition = var.norca_bus == null ? true : (
    var.norca_bus.account == "" ||
    contains([for u in var.users : u.account], var.norca_bus.account)
  )
  error_message = "norca_bus.account must be the account of one of the users."
}
