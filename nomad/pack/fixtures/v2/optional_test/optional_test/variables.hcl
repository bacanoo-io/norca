# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

variable "entrypoints" {
  type = list(object({
    name        = string
    protocol    = optional(string, "http")
    redirect_to = optional(string, "")
  }))
  default = []
}

variable "options" {
  type = object({
    mode  = string
    level = optional(number, 3)
  })
  default = { mode = "a" }
}
