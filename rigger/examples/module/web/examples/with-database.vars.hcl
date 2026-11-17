# Values for `rig install ./module/web --name shop -f examples/with-database.vars.hcl`.
# CI evaluates every file in examples/ and validates the jobs it gives.

count     = 3
log_level = "debug"

database = {
  service = "postgres"
  name    = "shop"
}

database_secret = "postgres/apps/shop"
