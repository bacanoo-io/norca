variable "namespace" {
  type    = string
  default = ""
}

variable "region" {
  type    = string
  default = ""
}

variable "datacenters" {
  type    = list(string)
  default = ["*"]

  validation {
    min_items       = 1
    item_min_length = 1
  }
}

variable "log_level" {
  type    = string
  default = "info"

  ui {
    options = ["debug", "info"]
  }
}

variable "ports" {
  type    = list(object({ name = string, protocol = string }))
  default = []

  ui {
    widget       = "object-list"
    item_options = { protocol = ["tcp", "udp"] }
  }
}

rule "region" {
  condition     = var.region == "" || var.region != "x"
  error_message = "not x."
}
