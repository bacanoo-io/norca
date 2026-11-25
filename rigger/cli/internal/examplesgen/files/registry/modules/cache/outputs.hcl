output "service" {
  value       = local.name
  description = "Name of the service in the catalog."
}

output "port" {
  value = var.port
}

# The shape of the variable "cache" of webapp: a stack passes it on as it
# is (cache = module.cache.connection).
output "connection" {
  description = "What a client needs: the service, the port and the secret with the password."
  value = {
    service         = local.name
    port            = var.port
    password_secret = var.password_secret
  }
}

output "next_steps" {
  value = <<EOT
Valkey ${local.name}: it requires the password of the secret ${var.password_secret}.
  norca secret put ${var.password_secret} password=-
EOT
}
