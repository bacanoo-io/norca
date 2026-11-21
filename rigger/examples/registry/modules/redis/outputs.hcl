output "service" {
  value       = var.job_name
  description = "Name of the job. The pack registers no service under it (README.md)."
}

output "port" {
  value       = var.port
  description = "Port Redis listens on inside its network namespace."
}

output "auth_variable" {
  value       = var.auth_enabled ? "nomad/jobs/redis" : null
  description = "Path of the Variable with the password; null without authentication."
}
