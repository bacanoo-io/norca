# A stack published in the registry, built on the registry's own modules.

stack {
  name        = "data-platform"
  version     = "1.2.0"
  description = "A PostgreSQL server and a NATS cluster for the applications of a namespace, with their secrets generated once."
  license     = "MPL-2.0"
  category    = "platform"
  keywords    = ["postgres", "nats", "database", "messaging"]

  requires {
    norca    = ">= 0.3.0"
    features = ["releases", "secret-store"]
  }
}

ui {
  tab "database" { label = "Database" }
  tab "bus" { label = "Message bus" }
}
