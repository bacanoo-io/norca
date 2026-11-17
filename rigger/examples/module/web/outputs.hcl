output "service" {
  value       = rig.release
  description = "Native service name of the web service."
}

output "next_steps" {
  description = "Shown after an install."
  value       = <<-EOT
    The service ${rig.release} runs ${var.count} instance(s) in namespace ${rig.namespace}.
      norca service info ${rig.release}
    %{ if var.database != null ~}
    It reads its database credentials from the secret ${var.database_secret}.
    %{ endif ~}
  EOT
}
