job "heartbeat" {
  id   = rigger.release
  name = rigger.release
  type = "batch"

  periodic {
    crons            = [var.schedule]
    prohibit_overlap = true
  }

  group "heartbeat" {
    secret_access {
      path = var.cache.password_secret
    }

    task "beat" {
      driver = "docker"

      # valkey-cli of the image of the cache counts the answers; wget of its
      # busybox asks the service.
      config {
        image   = "valkey/valkey:8.0-alpine"
        command = "sh"
        args    = ["-ec", file("files/beat.sh")]
      }

      env = {
        TARGET_NAME = var.target
        HEALTH_PATH = var.health_path
      }

      # Addresses from the service catalog and the password from the secret
      # store, when the task starts.
      template {
        destination = "secrets/heartbeat.env"
        env         = true
        data        = <<-EOT
          TARGET_ADDR=${service_address(var.target)}
          CACHE_ADDR=${service_address(var.cache.service)}
          VALKEYCLI_AUTH=${secret_ref(var.cache.password_secret, "password", { quote = false })}
          REDISCLI_AUTH=${secret_ref(var.cache.password_secret, "password", { quote = false })}
        EOT
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
