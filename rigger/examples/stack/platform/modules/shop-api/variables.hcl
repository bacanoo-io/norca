variable "image" {
  type    = string
  default = "registry.internal/shop/api:2.7.0"
}

variable "count" {
  type    = number
  default = 2
  validation {
    min     = 1
    integer = true
  }
}

# The shape of an element of the postgres module's output "databases".
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
