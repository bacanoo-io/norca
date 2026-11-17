# Values that differ between environments: `-f values/prod.vars.hcl`.
variable "node" {
  type        = string
  description = "Node with the host volume of the database."
}

variable "bus_servers" {
  type        = list(string)
  default     = []
  description = "Leaf node listeners of the Norca servers; empty: no change data capture."
}

variable "api_count" {
  type    = number
  default = 2
}
