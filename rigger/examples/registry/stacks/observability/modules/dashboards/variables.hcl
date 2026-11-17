variable "openobserve" {
  type = object({
    service = string
    secret  = string
  })
  description = "Native service of OpenObserve and the secret of its root user (items user, password)."
}

variable "sets" {
  type        = list(string)
  description = "Dashboard sets to load; each is a file files/<set>.json of this module."

  validation {
    min_items = 1
    unique    = true
  }
}

variable "image" {
  type    = string
  default = "curlimages/curl:8.11.0"
}
