locals {
  job              = rigger.release
  superuser_secret = "${var.secret_prefix}/superuser"
  app_secret       = { for d in var.databases : d.name => "${var.secret_prefix}/apps/${d.name}" }

  image = var.image_digest == "" ? var.image : "${var.image}@${var.image_digest}"

  # The settings as command line arguments: -c key=value, sorted.
  setting_args = flatten([for k in sort(keys(var.settings)) : ["-c", "${k}=${var.settings[k]}"]])
}

locals {
  # The SQL of one database, from data. format("%I") and ("%L") quote an
  # identifier and a literal inside the DO block.
  database_sql = {
    for d in var.databases : d.name => templatefile("files/database.sql.tftpl", {
      name       = d.name
      owner      = coalesce(d.owner, d.name)
      extensions = d.extensions
    })
  }
}
