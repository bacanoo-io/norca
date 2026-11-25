# The variables of a library are the arguments of the module block that
# calls it (module "sizing" { size = var.size }).

variable "size" {
  type        = string
  default     = "small"
  description = "The size of an instance."

  validation {
    one_of = ["small", "medium", "large"]
  }
}

variable "memory_max_factor" {
  type        = number
  default     = 1
  description = "memory_max as a multiple of memory: how far an instance may go over its reservation. 1 turns it off."

  validation {
    min = 1
    max = 4
  }
}
