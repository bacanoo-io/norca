# The inputs of the module: variable blocks, and rule blocks for what one
# variable alone can't say.
#
# A variable takes:
#   type        a type constraint: string, number, bool, list(T), set(T),
#               map(T), object({ a = T, b = optional(T, default) }), any
#               (the default when it is left out)
#   default     the value when none is given; without a default the
#               variable is required
#   description shown by rigger show, rigger search and the install form
#   sensitive   true: the value never appears in a plan, a log or a
#               revision; it may only go into an item of a Norca Variable
#               (settings.var.hcl), where it becomes a secret item
#   validation  checks of the value (one block)
#   ui          hints of the install form (one block)
#   secret      { path, item }: the value is read by the job from the
#               secret store at run time; the form names where and asks
#               for no value
#
# The constraints of a validation block:
#   string      pattern, min_length, max_length
#   number      min, max, integer
#   collection  min_items, max_items, unique, unique_by (an attribute of the
#               objects of a list), item_of (allowed items), item { ... }
#               (the constraints of every item)
#   map         key_pattern
#   object      attribute "<name>" { ... } (the constraints of an attribute)
#   any type    one_of (the allowed values; the form shows them as options)
#
# The hints of a ui block: tab, group, order, label, widget, unit, presets,
# option_labels, show_if (a condition over the other variables, written as
# a string), help, placeholder.
#
# The release's namespace and region are no variables: a module reads them
# as rigger.namespace and rigger.region.

# ---------------------------------------------------------------------------
# General
# ---------------------------------------------------------------------------

variable "image" {
  type        = string
  default     = "traefik/whoami:v1.10.3"
  description = "Container image of the service. Any image of an HTTP server that listens on port and answers health_path works."

  validation {
    pattern = "^[a-z0-9./_-]+(:[A-Za-z0-9._-]+)?$"
  }

  ui {
    tab         = "general"
    group       = "Image"
    order       = 1
    label       = "Container image"
    placeholder = "registry.example.com/team/app:1.0.0"
  }
}

variable "image_digest" {
  type        = string
  default     = ""
  description = "Digest of the image (sha256:...). With a digest the image is pinned to it; empty runs the tag."

  validation {
    pattern = "^(|sha256:[0-9a-f]{64})$"
  }

  ui {
    tab   = "general"
    group = "Image"
    order = 2
    help  = "docker buildx imagetools inspect <image> prints the digest."
  }
}

variable "args" {
  type        = list(string)
  default     = []
  description = "Arguments of the container, after the entrypoint of the image."

  ui {
    tab    = "general"
    group  = "Image"
    order  = 3
    widget = "tags"
  }
}

variable "env" {
  type        = map(string)
  default     = {}
  description = "More environment variables of the service. Never secrets: those come from the secret store."

  validation {
    key_pattern = "^[A-Z][A-Z0-9_]*$"
  }

  ui {
    tab    = "general"
    group  = "Environment"
    widget = "key-value"
  }
}

variable "log_level" {
  type        = string
  default     = "info"
  description = "Log level of the service; written into the settings Variable, which the task reads when it starts."

  validation {
    one_of = ["debug", "info", "warn", "error"]
  }

  ui {
    tab           = "general"
    group         = "Environment"
    widget        = "segmented"
    option_labels = { debug = "Debug", info = "Info", warn = "Warning", error = "Error" }
  }
}

variable "features" {
  type        = map(bool)
  default     = {}
  description = "Feature switches of the application, written into its configuration file (files/app.toml.tftpl)."

  validation {
    key_pattern = "^[a-z][a-z0-9_]*$"
    max_items   = 32
  }

  ui {
    tab    = "general"
    group  = "Environment"
    widget = "key-value"
  }
}

# ---------------------------------------------------------------------------
# Resources & Scaling
# ---------------------------------------------------------------------------

variable "count" {
  type        = number
  default     = 2
  description = "Instances of the service. With more than one they are spread over the datacenters."

  validation {
    min     = 1
    max     = 20
    integer = true
  }

  ui {
    tab    = "resources"
    group  = "Scaling"
    widget = "slider"
  }
}

variable "size" {
  type        = string
  default     = "small"
  description = "Size of an instance: small (100 MHz, 64 MiB), medium (500 MHz, 256 MiB), large (2000 MHz, 1 GiB). The library module lib/sizing turns it into CPU and memory."

  validation {
    one_of = ["small", "medium", "large"]
  }

  ui {
    tab           = "resources"
    group         = "Sizing"
    widget        = "segmented"
    option_labels = { small = "Small", medium = "Medium", large = "Large" }
  }
}

variable "memory_oversubscription" {
  type        = bool
  default     = false
  description = "Let an instance use up to twice its memory reservation (memory_max). Needs memory oversubscription on in the scheduler configuration of the cluster; without it the limit is ignored and the job is accepted with a warning."

  ui {
    tab    = "resources"
    group  = "Sizing"
    widget = "switch"
  }
}

variable "datacenters" {
  type        = list(string)
  default     = ["*"]
  description = "Datacenters the instances may run in; * is any."

  validation {
    min_items = 1
    unique    = true
  }

  ui {
    tab    = "resources"
    group  = "Placement"
    widget = "datacenter-multiselect"
  }
}

variable "node_pool" {
  type        = string
  default     = ""
  description = "Node pool of the instances; empty: the default pool."

  ui {
    tab    = "resources"
    group  = "Placement"
    widget = "node-pool-select"
  }
}

# ---------------------------------------------------------------------------
# Networking
# ---------------------------------------------------------------------------

variable "port" {
  type        = number
  default     = 8080
  description = "Port the service listens on inside its network namespace; the task finds it in NORCA_PORT_http too."

  validation {
    min     = 1024
    max     = 65535
    integer = true
  }

  ui {
    tab     = "networking"
    group   = "Listener"
    presets = [8080, 8000, 3000]
  }
}

variable "health_path" {
  type        = string
  default     = "/health"
  description = "HTTP path of the health check: an answer below 400 is healthy."

  validation {
    pattern = "^/[A-Za-z0-9/._-]*$"
  }

  ui {
    tab   = "networking"
    group = "Listener"
  }
}

variable "public" {
  type        = bool
  default     = false
  description = "Route requests for hostname to the service through Traefik (tags of the service the edge router reads)."

  ui {
    tab    = "networking"
    group  = "Route"
    widget = "switch"
  }
}

variable "hostname" {
  type        = string
  default     = ""
  description = "The host name of the route; needed with public."

  validation {
    pattern = "^(|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+)$"
  }

  ui {
    tab         = "networking"
    group       = "Route"
    placeholder = "app.example.com"
    show_if     = "public"
  }
}

variable "service_tags" {
  type        = list(string)
  default     = []
  description = "More tags of the service in the catalog."

  ui {
    tab    = "networking"
    group  = "Route"
    widget = "tags"
  }
}

# ---------------------------------------------------------------------------
# Security & Secrets
# ---------------------------------------------------------------------------

# A secret the job reads from the secret store at run time. The value of the
# variable is the path of the secret, never the secret; the secret block
# tells the install form where the secret is, so it asks for no value. A
# stack creates the secret once (secrets.hcl of a stack), or someone does:
#   norca secret put webapp/session key=-
variable "session_secret" {
  type        = string
  default     = "webapp/session"
  description = "Path of the secret with the item key, the key the service signs its sessions with."

  validation {
    pattern = "^[a-z0-9_-]+(/[a-z0-9_-]+)*$"
  }

  secret {
    path = "webapp/session"
    item = "key"
  }

  ui {
    tab    = "security"
    widget = "secret-path"
  }
}

# A sensitive value: it may go only into an item of a Norca Variable
# (settings.var.hcl), where it becomes a secret item of the release, never
# stored in a revision and never shown by a plan. Give it with
# RIGGER_VAR_api_token in the environment rather than in a file.
variable "api_token" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Token the service checks on its API; empty: no token."

  ui {
    tab   = "security"
    label = "API token"
  }
}

# ---------------------------------------------------------------------------
# Connections: filled by a stack from the outputs of other modules
# ---------------------------------------------------------------------------

variable "cache_enabled" {
  type        = bool
  default     = false
  description = "Use a cache: a release of the module cache (metadata.hcl requires one then)."

  ui {
    tab    = "connections"
    widget = "switch"
  }
}

# The shape of the output "connection" of the module cache, so a stack
# passes it as it is: cache = module.cache.connection.
variable "cache" {
  type = object({
    service         = optional(string, "cache")
    port            = optional(number, 6379)
    password_secret = optional(string, "cache/password")
  })
  default     = {}
  description = "The cache: its service in the catalog, its port and the secret with its password (item password)."

  validation {
    attribute "service" {
      pattern = "^[a-z0-9][a-z0-9-]*$"
    }

    attribute "port" {
      min     = 1
      max     = 65535
      integer = true
    }
  }

  ui {
    tab     = "connections"
    show_if = "cache_enabled"
  }
}

# ---------------------------------------------------------------------------
# Hooks
# ---------------------------------------------------------------------------

variable "smoke_test" {
  type        = bool
  default     = false
  description = "After every install and upgrade, a job asks the service for health_path; a failure fails the release."

  ui {
    tab    = "general"
    group  = "Checks"
    widget = "switch"
  }
}

# ---------------------------------------------------------------------------
# Rules: conditions over several variables, checked before anything is
# rendered. A failed rule stops the install with error_message.
# ---------------------------------------------------------------------------

rule "public_needs_a_hostname" {
  condition     = !var.public || var.hostname != ""
  error_message = "public needs hostname: the host name the route answers."
}

rule "hostname_only_when_public" {
  condition     = var.public || var.hostname == ""
  error_message = "hostname is used only with public = true."
}
