variable "datacenters" {
  type    = list(string)
  default = ["*"]
}

variable "node_pool" {
  type    = string
  default = ""
}

variable "spread_over" {
  type        = string
  default     = "datacenter"
  description = "What the instances are spread over: datacenter, node, or empty for none."

  validation {
    one_of = ["", "datacenter", "node"]
  }
}

variable "image" {
  type = string
}

variable "image_digest" {
  type    = string
  default = ""
}
