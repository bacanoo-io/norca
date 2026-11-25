# One Redis server. It began as the built-in pack redis 0.2.1 of norca,
# converted on paper in phase 0 of the design (design 14); since 0.4.0 the
# faults of that pack are fixed (README.md).

module {
  name        = "redis"
  version     = "0.4.2"
  description = "One Redis server: a service in the catalog, a password from a Norca Variable that the server requires, optional persistence on a host volume."
  url         = "https://redis.io"
  license     = "BSD-3-Clause" # Redis 7.2; the official image is referenced, not shipped
  category    = "database"
  keywords    = ["redis", "cache"]

  requires {
    norca    = ">= 0.1.0"
    features = ["releases", "secret-store"]
  }
}

# The tabs of the install form.
ui {
  tab "general" {
    label = "General"
    icon  = "sliders"
  }
  tab "resources" {
    label = "Resources & Scaling"
    icon  = "cpu"
  }
  tab "networking" {
    label = "Networking"
    icon  = "network"
  }
  tab "security" {
    label = "Security & Secrets"
    icon  = "lock"
  }
  tab "advanced" {
    label = "Advanced"
    icon  = "code"
  }
}
