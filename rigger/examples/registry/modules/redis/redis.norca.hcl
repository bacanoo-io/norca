# The job of the pack, written as a job specification: no template.
job "redis" {
  # The ID comes from the variable, as in the pack; the label is only the
  # name the module refers to the job by.
  id          = var.job_name
  region      = rig.region
  datacenters = var.datacenters
  node_pool   = var.node_pool
  type        = "service"

  group "redis" {
    count = var.count

    network {
      mode = var.network_mode

      port "db" {
        to = var.port
      }
    }

    task "redis" {
      driver = "docker"

      config {
        image = var.image
        ports = ["db"]
        # null leaves the attribute out, as the pack's [[ if ]] did.
        args = var.persistence_enabled ? ["redis-server", "--appendonly", "yes"] : null
      }

      resources {
        cpu    = var.cpu
        memory = var.memory
      }
    }
  }
}
