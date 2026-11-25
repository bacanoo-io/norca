# The inputs of the stack, as a module has them: whoever installs the
# published stack gives these, not the variables of its modules.

variable "web_count" {
  type        = number
  default     = 2
  description = "Instances of the web service."

  validation {
    min     = 1
    max     = 20
    integer = true
  }

  ui {
    tab    = "web"
    widget = "slider"
  }
}

variable "web_size" {
  type        = string
  default     = "small"
  description = "Size of an instance of the web service."

  validation {
    one_of = ["small", "medium", "large"]
  }

  ui {
    tab = "web"
  }
}

variable "hostname" {
  type        = string
  default     = ""
  description = "The host name of the route through Traefik; empty: no route."

  ui {
    tab         = "web"
    placeholder = "app.example.com"
  }
}

variable "cache_memory" {
  type        = number
  default     = 256
  description = "Memory of the cache in MiB."

  validation {
    min     = 64
    integer = true
  }

  ui {
    tab  = "cache"
    unit = "MiB"
  }
}
