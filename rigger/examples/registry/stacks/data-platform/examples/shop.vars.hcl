# rigger stack apply example/data-platform --version "~> 1.2" -f shop.vars.hcl

database_node = "worker-1"

databases = [
  { name = "shop", extensions = ["pgcrypto"] },
  { name = "reports", owner = "reporting" },
]

bus_applications = ["shop", "billing"]
