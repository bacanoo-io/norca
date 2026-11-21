variable "size" {
  type        = string
  default     = "small"
  description = "small: one store and one OpenObserve instance. cluster: a distributed store and OpenObserve in node roles."

  validation {
    one_of = ["small", "cluster"]
  }
}

variable "storage_volume" {
  type        = string
  default     = "rustfs"
  description = "Host volume of the S3 store."
}

variable "retention_days" {
  type        = number
  default     = 15
  description = "How long logs, metrics and traces are kept."

  validation {
    min     = 1
    max     = 365
    integer = true
  }

  ui { widget = "slider" }
}

variable "collect" {
  type        = list(string)
  default     = ["host", "norca"]
  description = "What the collectors read: host, norca, nats, postgres, traefik."

  validation {
    unique  = true
    item_of = ["host", "norca", "nats", "postgres", "traefik"]
  }
}

variable "dashboards" {
  type        = list(string)
  default     = ["cluster", "nodes"]
  description = "Dashboard sets loaded into OpenObserve."

  validation {
    unique  = true
    item_of = ["cluster", "nodes", "nats", "postgres"]
  }
}

rule "dashboards_need_their_source" {
  condition = alltrue([
    for d in var.dashboards : contains(["cluster", "nodes"], d) || contains(var.collect, d)
  ])
  error_message = "a dashboard set of a service (nats, postgres) needs the same name in collect."
}
