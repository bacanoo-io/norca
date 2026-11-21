# What a stack that builds on this one, or an application module, needs.

output "databases" {
  value       = module.postgres.databases
  description = "Per database: the service, the database name and the secret of its owner."
}

output "bus" {
  description = "Per application: how it connects to the bus."
  value = {
    for app in var.bus_applications : app => {
      service = module.nats.service
      port    = module.nats.client_port
      user    = app
      secret  = "nats/users/${app}"
    }
  }
}
