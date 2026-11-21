output "service" {
  value       = rigger.release
  description = "Native service name of the web service."
}

output "next_steps" {
  description = "Shown after an install."
  value       = <<-EOT
    The service ${rigger.release} runs ${var.count} instance(s) in namespace ${rigger.namespace}.
      norca service info ${rigger.release}
    %{if var.database != null~}
    It reads its database credentials from the secret ${var.database_secret}.
    %{endif~}
  EOT
}
