# One group per subject: each scales and restarts on its own.
job "worker" {
  id   = rig.release
  name = rig.release
  type = "service"

  dynamic "group" {
    for_each = var.subjects
    # "shop.orders.created" -> "orders-created"
    labels = [replace(trimprefix(group.value, "shop."), ".", "-")]

    content {
      count = var.workers_per_subject

      secret_access {
        path = var.database.secret
      }

      secret_access {
        path = var.bus.secret
      }

      task "worker" {
        driver = "docker"

        config {
          image = var.image
        }

        env = {
          SUBJECT   = group.value
          DB_NAME   = var.database.name
          NATS_USER = var.bus.user
        }

        template {
          destination = "secrets/connections.env"
          env         = true
          data        = <<-EOT
            DB_ADDR=${service_address(var.database.service)}
            DB_USER=${secret_ref(var.database.secret, "user", { quote = false })}
            DB_PASSWORD=${secret_ref(var.database.secret, "password", { quote = false })}
            NATS_URL=nats://${service_address(var.bus.service)}
            NATS_PASSWORD=${secret_ref(var.bus.secret, "password", { quote = false })}
          EOT
        }

        resources {
          cpu    = 200
          memory = 128
        }
      }
    }
  }
}
