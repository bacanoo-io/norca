# A second module of the registry: the cache that webapp may use. A stack
# installs both and passes the output "connection" of this module to
# webapp (stacks/web-platform, ../../../stack/shop).

module {
  name        = "cache"
  version     = "0.1.0"
  description = "One Valkey server as a cache: a password from the secret store, optional persistence on a host volume."
  url         = "https://valkey.io"
  license     = "BSD-3-Clause" # Valkey; the official image is referenced, not shipped
  category    = "database"
  keywords    = ["cache", "valkey", "redis", "example"]

  requires {
    norca    = ">= 0.3.0"
    features = ["releases", "secret-store"]
  }
}

ui {
  tab "general" {
    label = "General"
  }

  tab "storage" {
    label = "Storage"
  }
}
