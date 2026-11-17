# A stack of this registry built on the modules of ANOTHER registry: the
# registry adds no module for the software, only the way they are put
# together, and one small module of its own.

stack {
  name        = "observability"
  version     = "0.4.0"
  description = "Logs, metrics and traces of a cluster: an S3 store, OpenObserve on it, and a collector on every node; modules of the norca registry, wired and sized."
  license     = "MPL-2.0"
  category    = "observability"
  keywords    = ["logs", "metrics", "traces", "openobserve", "opentelemetry"]
  icon        = "icon.png"

  requires {
    norca    = ">= 0.3.0"
    features = ["releases", "secret-store"]
  }
}
