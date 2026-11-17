output "service" {
  value       = local.job
  description = "Native service name of the server."
}

output "port" {
  value = var.port
}

# What an application needs to use one of the databases: the stack passes an
# element of this map to the module of the application.
output "databases" {
  description = "Per database: the service, the database name and the secret with the owner's credentials."
  value = {
    for d in var.databases : d.name => {
      service = local.job
      name    = d.name
      secret  = local.app_secret[d.name]
    }
  }
}

output "next_steps" {
  value = <<-EOT
    PostgreSQL ${local.job} on node ${var.node}, port ${var.port}.
      norca secret put ${local.superuser_secret} password=-
    %{ for d in var.databases ~}
      norca secret put ${local.app_secret[d.name]} user=${coalesce(d.owner, d.name)} password=-
    %{ endfor ~}
    %{ if var.backup.enabled ~}
    Dumps: ${var.backup.schedule} to bucket ${var.backup.s3.bucket}, kept ${var.backup.keep_days} days.
    %{ endif ~}
  EOT
}
