# A small module: one web service behind a native service with checks.

module {
  name        = "web"
  version     = "0.1.0"
  description = "A stateless web service with health checks, optionally using a database and a message bus."
  url         = "https://example.org/web"
  license     = "MPL-2.0" # of the software the module installs
  category    = "application"
  keywords    = ["web", "http"]

  requires {
    norca    = ">= 0.3.0"
    features = ["releases"]
  }
}
