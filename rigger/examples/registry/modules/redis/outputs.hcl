output "service" {
  value       = var.job_name
  description = "Name of the service in the catalog; its address and port are those of Redis on the node."
}

output "port" {
  value       = var.port
  description = "Port Redis listens on in the network namespace of the group (bridge mode)."
}

output "auth_variable" {
  value       = var.auth_enabled ? local.auth_path : null
  description = "Path of the Variable with the user and the password; null without authentication."
}
