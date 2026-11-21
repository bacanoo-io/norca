# The built-in pack redis 0.2.1 of norca (norca/norca/norca/pack_builtin_catalog.go),
# converted on paper in phase 0 of the design. It follows the pack, its faults
# included, so that it renders the same job (design 14); the faults are named
# in the comments and in README.md.

module {
  name        = "redis"
  version     = "0.3.0"
  description = "An in-memory data store with optional AOF persistence and a password in a Norca Variable."
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
