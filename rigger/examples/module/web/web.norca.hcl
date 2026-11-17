job "web" {
  name        = local.name
  type        = "service"
  datacenters = var.datacenters

  update {
    max_parallel     = 1
    min_healthy_time = "10s"
    auto_revert      = true
  }

  group "web" {
    count = var.count

    network {
      port "http" {
        to = 8080
      }
    }

    # Only with a database: the task may read its credentials.
    dynamic "secret_access" {
      for_each = var.database == null ? [] : [var.database_secret]
      content {
        path = secret_access.value
      }
    }

    service {
      name     = local.name
      port     = "http"
      provider = "nomad"

      check {
        name     = "ready"
        role     = "readiness"
        type     = "http"
        path     = "/healthz"
        interval = "10s"
        timeout  = "2s"
      }

      check {
        name     = "alive"
        role     = "liveness"
        type     = "http"
        path     = "/livez"
        interval = "30s"
        timeout  = "2s"
      }
    }

    task "web" {
      driver = "docker"

      config {
        image = image_ref(var.image, var.image_digest)
        ports = ["http"]
      }

      env = local.env

      # The credentials reach the task at run time; the {{ }} below is the
      # task template of the cluster, Rig only fills in the path.
      dynamic "template" {
        for_each = var.database == null ? [] : [var.database_secret]
        content {
          destination = "secrets/db.env"
          env         = true
          data        = <<-EOT
            {{ with nomadSecret "${template.value}" }}
            DB_USER={{ .user }}
            DB_PASSWORD={{ .password }}
            {{ end }}
          EOT
        }
      }

      resources {
        cpu    = var.resources.cpu
        memory = var.resources.memory
      }
    }
  }
}
