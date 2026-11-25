# The inputs of the stack: values per environment come from the files of
# values/ (-f values/prod.vars.hcl), from RIGGER_VAR_<name> in the
# environment and from --var name=value, the last one winning.

variable "web_image" {
  type        = string
  default     = "traefik/whoami:v1.10.3"
  description = "The image of the web service."
}

variable "web_count" {
  type        = number
  default     = 1
  description = "Instances of the web service."

  validation {
    min     = 1
    integer = true
  }
}

variable "environment" {
  type        = string
  default     = "dev"
  description = "The environment: dev or prod. It decides the size of the web service."

  validation {
    one_of = ["dev", "prod"]
  }
}

variable "hostname" {
  type        = string
  default     = ""
  description = "The host name of the route of the web service; empty: no route."
}

variable "features" {
  type        = map(bool)
  default     = {}
  description = "The feature switches of the web service."
}

variable "smoke_test" {
  type        = bool
  default     = false
  description = "Check the web service after every install and upgrade."
}

variable "cache_memory" {
  type        = number
  default     = 128
  description = "Memory of the cache in MiB."
}

variable "cache_persistence" {
  type        = bool
  default     = false
  description = "Keep the data of the cache on the host volume cache."
}

variable "heartbeat_schedule" {
  type        = string
  default     = "*/5 * * * *"
  description = "Cron schedule of the heartbeat job; empty: no heartbeat."
}
