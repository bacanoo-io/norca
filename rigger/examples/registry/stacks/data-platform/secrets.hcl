# The secrets of the stack, created once with generated values and kept on a
# later apply. `for_each` makes one per database and per application.

secret "postgres/superuser" {
  items = { password = "!generate:postgres-superuser" }
}

secret "database" {
  for_each = { for d in var.databases : d.name => d }

  path = "postgres/apps/${each.key}"
  items = {
    user     = coalesce(each.value.owner, each.key)
    password = "!generate:postgres-${each.key}"
  }
}

secret "bus-user" {
  for_each = toset(var.bus_applications)

  path  = "nats/users/${each.key}"
  items = { password = "!generate:nats-${each.key}" }
}
