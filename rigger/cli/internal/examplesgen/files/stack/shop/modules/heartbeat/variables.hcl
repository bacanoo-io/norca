variable "schedule" {
  type        = string
  description = "Cron schedule of the job."

  validation {
    min_length = 9
  }
}

variable "target" {
  type        = string
  description = "The service in the catalog that is asked."
}

variable "port" {
  type        = number
  default     = 8080
  description = "Port of the target, for the record."
}

variable "health_path" {
  type    = string
  default = "/health"
}

# The shape of the output "connection" of the module cache.
variable "cache" {
  type = object({
    service         = string
    port            = number
    password_secret = string
  })
  description = "The cache the answers are counted in."
}
