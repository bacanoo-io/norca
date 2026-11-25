# Outputs: what the module tells whoever installs it, and what a stack
# passes on to other modules (module.<release>.<output>). They are known at
# install time; an output can't read the running cluster. An output that
# takes a sensitive value says sensitive = true.

output "service" {
  value       = local.name
  description = "Name of the service in the catalog."
}

output "port" {
  value       = var.port
  description = "Port of the service inside its network namespace."
}

output "url" {
  value       = var.public ? "https://${var.hostname}" : null
  description = "The address of the route; null without one."
}

output "settings_variable" {
  value       = local.settings_path
  description = "Path of the Norca Variable with the settings."
}

# output "api_token" {
#   value     = var.api_token
#   sensitive = true
# }

# An output named next_steps is shown after an install. The template syntax
# of HCL writes lines conditionally (%{ if }) and repeatedly (%{ for }); ~
# strips the line end after a directive. The heredoc is written from the
# first column (<<EOT, not <<-EOT), so a directive leaves no indentation
# behind.
output "next_steps" {
  description = "Shown after an install."
  value       = <<EOT
${local.name}: ${var.count} instance(s) in the namespace ${rigger.namespace}.
  norca service info ${local.name}
  norca var get ${local.settings_path}
%{if var.public~}
Route: https://${var.hostname} through Traefik.
%{endif~}
The session key is read from the secret ${var.session_secret} (item key):
  norca secret put ${var.session_secret} key=-
EOT
}
