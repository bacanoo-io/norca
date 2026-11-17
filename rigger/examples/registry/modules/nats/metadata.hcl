module {
  name        = "nats"
  version     = "1.0.0"
  description = "A NATS cluster per region for the applications: JetStream, users in accounts, TLS, and optionally the changes of the Norca region as a leaf node of the system bus."
  url         = "https://nats.io"
  license     = "Apache-2.0" # NATS server; the official image is referenced, not shipped
  category    = "messaging"
  keywords    = ["nats", "jetstream", "messaging", "cdc"]
  icon        = "icon.png" # 64x64 PNG

  requires {
    norca    = ">= 0.3.0"
    features = ["releases", "secret-store"]
  }
}

# The tabs of the install form.
ui {
  tab "general" { label = "General" }
  tab "cluster" { label = "Cluster" }
  tab "auth" { label = "Users and TLS" }
  tab "cdc" { label = "Norca changes" }
}
