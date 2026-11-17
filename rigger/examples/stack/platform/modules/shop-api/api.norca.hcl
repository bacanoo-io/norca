job "api" {
  name = local.name
  type = "service"

  update {
    max_parallel = 1
    canary       = 1
    auto_promote = true
    auto_revert  = true
  }

  group "api" {
    count = var.count

    network {
      port "http" {
        to = 8080
      }
    }

    secret_access {
      path = var.database.secret
    }

    secret_access {
      path = var.bus.secret
    }

    service {
      name     = local.name
      port     = "http"
      provider = "nomad"
      tags     = ["traefik.enable=true", "traefik.http.routers.${local.name}.rule=PathPrefix(`/api`)"]

      check {
        name     = "ready"
        role     = "readiness"
        type     = "http"
        path     = "/healthz"
        interval = "10s"
        timeout  = "2s"
      }
    }

    task "api" {
      driver = "docker"

      config {
        image = var.image
        ports = ["http"]
      }

      env = {
        DB_NAME        = var.database.name
        NATS_USER      = var.bus.user
        ORDER_SUBJECTS = join(",", local.order_subjects)
      }

      # Addresses from the native catalog and credentials from the secret
      # store, both resolved on the node when the task starts.
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
        cpu    = 300
        memory = 256
      }
    }
  }
}
