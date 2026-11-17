variable "database_node" {
  type        = string
  description = "Name of the node with the host volume of the database."
  validation { min_length = 1 }
  ui { tab = "database" }
}

variable "databases" {
  type = list(object({
    name       = string
    owner      = optional(string, "")
    extensions = optional(list(string), [])
  }))
  default     = []
  description = "Application databases; each gets an owner with a generated password."

  validation {
    unique_by = "name"
    attribute "name" { pattern = "^[a-z][a-z0-9_]{0,62}$" }
  }

  ui {
    tab    = "database"
    widget = "object-list"
  }
}

variable "database_settings" {
  type        = map(string)
  default     = {}
  description = "postgresql.conf settings."
  ui {
    tab    = "database"
    widget = "code"
  }
}

variable "bus_servers" {
  type        = number
  default     = 3
  description = "Servers of the NATS cluster: 1, or an odd number."
  validation {
    one_of = [1, 3, 5]
  }
  ui { tab = "bus" }
}

variable "bus_applications" {
  type        = list(string)
  default     = []
  description = "Applications that use the bus; each gets a user in its own account with a generated password."

  validation {
    unique       = true
    item_pattern = "^[a-z][a-z0-9_-]{0,31}$"
  }

  ui { tab = "bus" }
}
