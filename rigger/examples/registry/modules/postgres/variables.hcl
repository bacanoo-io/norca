variable "image" {
  type        = string
  default     = "postgres:17.2-alpine"
  description = "The PostgreSQL image. The major version must match the data on the volume."
  ui { tab = "general" }
}

variable "image_digest" {
  type        = string
  default     = ""
  description = "Digest of the image; empty runs the tag."
  validation { pattern = "^(|sha256:[0-9a-f]{64})$" }
  ui { tab = "general" }
}

variable "node" {
  type        = string
  description = "Name of the node that has the host volume: the server runs there and nowhere else."
  validation { min_length = 1 }
  ui { tab = "general" }
}

variable "volume" {
  type        = string
  default     = "postgres"
  description = "Host volume of the data directory."
  ui { tab = "general" }
}

variable "port" {
  type        = number
  default     = 5432
  description = "Static port of the server."
  validation {
    min     = 1
    max     = 65535
    integer = true
  }
  ui { tab = "general" }
}

variable "resources" {
  type = object({
    cpu    = optional(number, 500)
    memory = optional(number, 1024)
  })
  default     = {}
  description = "CPU (MHz) and memory (MiB) of the server."
  ui { tab = "general" }
}

variable "settings" {
  type        = map(string)
  default     = {}
  description = "postgresql.conf settings, e.g. { max_connections = \"200\" }."

  validation {
    # keys of a map
    key_pattern = "^[a-z][a-z0-9_.]*$"
  }

  ui {
    tab    = "general"
    widget = "code"
  }
}

variable "secret_prefix" {
  type        = string
  default     = "postgres"
  description = "Prefix of the secrets: <prefix>/superuser (item password) and <prefix>/apps/<database> (items user, password)."
  ui {
    tab    = "general"
    widget = "secret-path"
  }
}

variable "databases" {
  type = list(object({
    name       = string
    owner      = optional(string, "")
    extensions = optional(list(string), [])
  }))
  default     = []
  description = "Application databases. The hook creates each with its owner (empty: named after the database) and extensions; the owner's password is the item password of <secret_prefix>/apps/<name>."

  validation {
    unique_by = "name"
    attribute "name" { pattern = "^[a-z][a-z0-9_]{0,62}$" }
    attribute "owner" { pattern = "^(|[a-z][a-z0-9_]{0,62})$" }
    attribute "extensions" {
      unique = true

      item {
        pattern = "^[a-z][a-z0-9_-]*$"
      }
    }
  }

  ui {
    tab    = "databases"
    widget = "object-list"
  }
}

variable "backup" {
  type = object({
    enabled   = optional(bool, false)
    schedule  = optional(string, "15 2 * * *")
    s3        = optional(object({ service = string, bucket = string }))
    s3_secret = optional(string, "postgres/backup-s3")
    keep_days = optional(number, 14)
  })
  default     = {}
  description = "Periodic pg_dumpall to an S3 store: schedule (cron), the S3 service and bucket, the secret with the items access_key and secret_key, how long dumps are kept."

  validation {
    attribute "keep_days" {
      min     = 1
      integer = true
    }
  }

  ui { tab = "backup" }
}

rule "backup_needs_s3" {
  condition     = !var.backup.enabled || var.backup.s3 != null
  error_message = "backup.enabled needs backup.s3: the service and bucket of the S3 store."
}

rule "reserved_database_names" {
  condition     = length(setintersection([for d in var.databases : d.name], ["postgres", "template0", "template1"])) == 0
  error_message = "the databases postgres, template0 and template1 belong to the server."
}
