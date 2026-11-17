# A server with two application databases and nightly dumps.
#   norca secret put postgres/superuser password=-
#   norca secret put postgres/apps/shop user=shop password=-
#   norca secret put postgres/apps/reports user=reporting password=-
#   norca secret put postgres/backup-s3 access_key=- secret_key=-

node = "worker-1"

settings = {
  max_connections = "200"
  shared_buffers  = "256MB"
}

databases = [
  { name = "shop", extensions = ["pgcrypto"] },
  { name = "reports", owner = "reporting" },
]

backup = {
  enabled   = true
  schedule  = "30 1 * * *"
  s3        = { service = "rustfs", bucket = "backups" }
  keep_days = 30
}
