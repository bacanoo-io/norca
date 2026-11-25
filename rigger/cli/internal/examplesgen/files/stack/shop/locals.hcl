# Locals and checks of the stack, as in a module.

locals {
  web_size = var.environment == "prod" ? "medium" : "small"
}

check "prod_has_a_route" {
  condition     = var.environment != "prod" || var.hostname != ""
  error_message = "prod needs hostname: the web service is reached through its route."
}
