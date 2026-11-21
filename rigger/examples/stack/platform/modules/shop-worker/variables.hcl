variable "image" {
  type    = string
  default = "registry.internal/shop/worker:2.7.0"
}

variable "database" {
  type = object({
    service = string
    name    = string
    secret  = string
  })
}

variable "bus" {
  type = object({
    service = string
    port    = number
    user    = string
    secret  = string
  })
}

variable "subjects" {
  type = list(string)
  validation {
    min_items = 1
    unique    = true
  }
}

variable "workers_per_subject" {
  type    = number
  default = 1
}
