# A published stack: metadata.hcl with a stack block makes a stack of the
# registry, versioned and tagged like a module (web-platform/v1.0.0) and
# signed by "rigger stack publish". The block takes the fields of a module
# block (name, version, description, url, license, category, keywords,
# icon, requires); the name must be the name of the rigger block of
# stack.hcl. A stack without metadata.hcl is a stack of one's own, never
# published (../../../stack/shop).

stack {
  name        = "web-platform"
  version     = "1.0.0"
  description = "The web service of the registry with its cache: one apply installs both, wired, with their secrets generated once."
  license     = "Apache-2.0"
  category    = "application"
  keywords    = ["web", "cache", "example"]

  requires {
    norca    = ">= 0.3.0"
    features = ["releases", "release-hooks", "secret-store"]
  }
}

# The tabs of the install form of the stack in the web UI.
ui {
  tab "web" {
    label = "Web service"
  }

  tab "cache" {
    label = "Cache"
  }
}
