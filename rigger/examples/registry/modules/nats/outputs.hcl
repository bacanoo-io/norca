output "service" {
  value       = local.job
  description = "Native service name of the client port."
}

output "client_port" {
  value = var.ports.client
}

output "accounts" {
  value       = local.accounts
  description = "The NATS accounts of the users."
}

output "cdc_domain" {
  value       = var.norca_bus == null ? null : "norca-${rigger.region}"
  description = "JetStream domain of the consumers the Norca servers keep for this NATS; null without norca_bus."
}

output "next_steps" {
  value = <<-EOT
    NATS ${local.job}: ${var.count} server(s) on port ${var.ports.client} in region ${rigger.region}.
    %{for u in var.users~}
      user ${u.name} (account ${u.account}): norca secret put ${var.users_secret_prefix}/${u.name} password=-
    %{endfor~}
    %{if var.norca_bus != null~}
    Changes of the region arrive in ${local.bus_account == "" ? "the global account" : "account ${local.bus_account}"}.
      norca secret put ${var.norca_bus.secret} token=<secret ID> ca=@ca.pem
    %{endif~}
  EOT
}
