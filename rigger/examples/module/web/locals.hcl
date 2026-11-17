locals {
  name = rig.release

  # Environment built from data: only what is configured appears.
  env = merge(
    {
      LOG_LEVEL = var.log_level
      HTTP_ADDR = "0.0.0.0:8080"
    },
    var.database == null ? {} : {
      DB_SERVICE = var.database.service
      DB_NAME    = var.database.name
    },
    var.bus == null ? {} : {
      NATS_SERVICE = var.bus.service
    },
  )
}
