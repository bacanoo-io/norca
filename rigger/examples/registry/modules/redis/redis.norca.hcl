# One Redis server (the official image is referenced, not shipped). No
# replication and no failover: with persistence the data is on one host
# volume, without it Redis is a cache that starts empty.
job "redis" {
  # The ID comes from the variable; the label is only the name the module
  # refers to the job by.
  id          = var.job_name
  region      = rigger.region
  datacenters = var.datacenters
  node_pool   = var.node_pool
  type        = "service"

  group "redis" {
    # One server: several would be unrelated stores behind one service name.
    count = 1

    network {
      mode = var.network_mode

      port "db" {
        # null leaves the attribute out: in host mode there is no mapping,
        # Redis listens on the port the node gives.
        to = var.network_mode == "host" ? null : var.port
      }
    }

    dynamic "volume" {
      for_each = var.persistence_enabled ? [var.host_volume] : []
      labels   = ["data"]

      content {
        type      = "host"
        source    = volume.value
        read_only = false
      }
    }

    service {
      name     = var.job_name
      provider = "nomad"
      port     = "db"
      tags     = var.service_tags

      # Native checks are HTTP or TCP: the server accepts connections.
      check {
        name     = "readiness"
        role     = "readiness"
        type     = "tcp"
        interval = "10s"
        timeout  = "3s"
      }

      check {
        name     = "liveness"
        role     = "liveness"
        type     = "tcp"
        interval = "15s"
        timeout  = "3s"

        check_restart {
          limit = 3
          grace = "30s"
        }
      }
    }

    task "redis" {
      driver = "docker"

      config {
        image = var.image
        ports = ["db"]
        # The entrypoint of the image stays: for redis-server it makes the
        # user redis the owner of /data and runs the server as that user.
        args = ["redis-server", "/secrets/redis.conf"]
      }

      dynamic "volume_mount" {
        for_each = var.persistence_enabled ? ["data"] : []

        content {
          volume      = volume_mount.value
          destination = "/data"
        }
      }

      # The configuration, with the password when authentication is on: in
      # the secrets directory of the task, readable by the user of the
      # server alone. A changed password restarts the server.
      template {
        data        = local.config
        destination = "secrets/redis.conf"
        uid         = var.uid
        perms       = "0400"
      }

      env = var.extra_env

      resources {
        cpu    = var.cpu
        memory = var.memory
      }
    }
  }
}
