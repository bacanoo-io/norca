# One Valkey server. No replication: with persistence the data is on one
# host volume, without it the cache starts empty.

job "cache" {
  id   = local.name
  name = local.name
  type = "service"

  group "cache" {
    count = 1

    network {
      port "db" {
        to = var.port
      }
    }

    # A volume only with persistence: a dynamic block with a labels list.
    dynamic "volume" {
      for_each = var.persistence ? [var.host_volume] : []
      labels   = ["data"]

      content {
        type      = "host"
        source    = volume.value
        read_only = false
      }
    }

    secret_access {
      path = var.password_secret
    }

    service {
      name     = local.name
      port     = "db"
      provider = "nomad"
      tags     = ["cache"]

      check {
        name     = "ready"
        role     = "readiness"
        type     = "tcp"
        interval = "10s"
        timeout  = "3s"
      }
    }

    task "valkey" {
      driver = "docker"

      config {
        image = var.image
        ports = ["db"]
        args  = ["valkey-server", "/secrets/valkey.conf"]
      }

      dynamic "volume_mount" {
        for_each = var.persistence ? ["data"] : []

        content {
          volume      = volume_mount.value
          destination = "/data"
        }
      }

      # The configuration with the password, readable by the task alone. A
      # changed password restarts the server.
      template {
        destination = "secrets/valkey.conf"
        data        = local.config
        perms       = "0400"
        uid         = 999 # the user valkey of the official image
      }

      resources {
        cpu    = 200
        memory = var.memory
      }
    }
  }
}
