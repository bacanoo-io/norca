job "postgres" {
  id   = local.job
  name = local.job
  type = "service"

  # The data is on one node.
  constraint {
    attribute = runtime("node.unique.name")
    value     = var.node
  }

  update {
    max_parallel     = 1
    health_check     = "checks"
    min_healthy_time = "10s"
    healthy_deadline = "5m"
  }

  group "postgres" {
    count = 1

    volume "data" {
      type      = "host"
      source    = var.volume
      read_only = false
    }

    network {
      port "db" {
        static = var.port
      }
    }

    secret_access {
      path = local.superuser_secret
    }

    service {
      name     = local.job
      port     = "db"
      provider = "nomad"

      check {
        name     = "ready"
        role     = "readiness"
        type     = "script"
        task     = "postgres"
        command  = "pg_isready"
        args     = ["-U", "postgres", "-p", tostring(var.port)]
        interval = "10s"
        timeout  = "3s"
      }
    }

    task "postgres" {
      driver = "docker"

      config {
        image = local.image
        ports = ["db"]
        args  = concat(["-p", tostring(var.port)], local.setting_args)
      }

      volume_mount {
        volume      = "data"
        destination = "/var/lib/postgresql/data"
      }

      template {
        destination = "secrets/superuser.env"
        env         = true
        data        = "POSTGRES_PASSWORD=${secret_ref(local.superuser_secret, "password", { quote = false })}"
      }

      # A clean stop: postgres finishes its transactions.
      kill_signal  = "SIGINT"
      kill_timeout = "120s"

      resources {
        cpu    = var.resources.cpu
        memory = var.resources.memory
      }
    }
  }
}
