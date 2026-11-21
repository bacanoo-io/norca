# Secrets the stack creates once (kept on a second apply unless overwrite).
secret "postgres/superuser" {
  items = { password = "!generate:postgres-superuser" }
}

secret "postgres/apps/shop" {
  items = {
    user     = "shop"
    password = "!generate:postgres-shop"
  }
}

secret "postgres/apps/reports" {
  items = {
    user     = "reporting"
    password = "!generate:postgres-reports"
  }
}

secret "nats/users/shop" {
  items = { password = "!generate:nats-shop" }
}

secret "nats/users/audit" {
  items = { password = "!generate:nats-audit" }
}
