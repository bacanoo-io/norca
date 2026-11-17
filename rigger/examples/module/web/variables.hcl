variable "image" {
  type        = string
  default     = "ghcr.io/example/web:1.4.2"
  description = "Container image of the service."

  validation {
    pattern = "^[a-z0-9./_-]+:[A-Za-z0-9._-]+$"
  }

  ui {
    tab   = "general"
    group = "Image"
  }
}

variable "image_digest" {
  type        = string
  default     = ""
  description = "Digest of the image (sha256:...). Empty runs the tag."

  validation {
    pattern = "^(|sha256:[0-9a-f]{64})$"
  }

  ui {
    tab   = "general"
    group = "Image"
  }
}

variable "count" {
  type        = number
  default     = 2
  description = "Instances of the service."

  validation {
    min     = 1
    max     = 50
    integer = true
  }

  ui {
    tab    = "general"
    widget = "slider"
  }
}

variable "datacenters" {
  type        = list(string)
  default     = ["*"]
  description = "Datacenters the instances may run in."

  validation {
    min_items = 1
  }
}

variable "resources" {
  type = object({
    cpu    = optional(number, 200)
    memory = optional(number, 128)
  })
  default     = {}
  description = "CPU (MHz) and memory (MiB) of an instance."

  ui {
    tab = "resources"
  }
}

variable "log_level" {
  type        = string
  default     = "info"
  description = "Log level of the service."

  validation {
    one_of = ["debug", "info", "warn", "error"]
  }
}

# Where the service finds what it depends on. A stack fills these from the
# outputs of other modules.

variable "database" {
  type = object({
    service = string
    name    = string
  })
  default     = null
  description = "Native service name and database name of a PostgreSQL the service uses."

  ui {
    tab = "connections"
  }
}

variable "database_secret" {
  type        = string
  default     = ""
  description = "Path of the secret with the database credentials (items user, password)."

  # The value is a path in the secret store, never the secret.
  validation {
    pattern = "^([a-z0-9_-]+/)*[a-z0-9_-]*$"
  }

  ui {
    tab    = "connections"
    widget = "secret-path"
  }
}

variable "bus" {
  type = object({
    service = string
    account = optional(string, "")
  })
  default     = null
  description = "Native service name of a NATS the service connects to."

  ui {
    tab = "connections"
  }
}

rule "database_needs_its_secret" {
  condition     = var.database == null || var.database_secret != ""
  error_message = "database needs database_secret: the path of the secret with the credentials."
}
