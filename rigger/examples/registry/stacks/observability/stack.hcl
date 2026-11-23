rigger {
  required_version = ">= 0.1.0"
  name             = "observability" # the name metadata.hcl publishes the stack by

  # The external registry the modules come from.
  registry "norca" {
    source = "github.com/bacanoo-io/norca-registry"
  }

  # Its modules must be signed by its key; the key travels with this stack,
  # so whoever trusts the stack's signature gets the right key for the
  # modules too.
  trust {
    registry          = "norca"
    require_signature = true
    public_keys       = [file("keys/norca-registry.pub")]
  }
}

# ---------------------------------------------------------------------------
# Modules of the external registry
# ---------------------------------------------------------------------------

module "rustfs" {
  source  = "norca/rustfs"
  version = "~> 0.2"

  values = {
    count   = var.size == "small" ? 1 : 4
    buckets = ["openobserve"]
    volume  = var.storage_volume
  }
}

module "openobserve" {
  source  = "norca/openobserve"
  version = "~> 0.2"

  values = {
    mode = var.size == "small" ? "single" : "cluster"

    # The store of the first module, through its outputs.
    s3 = {
      service = module.rustfs.service
      bucket  = "openobserve"
      secret  = module.rustfs.credentials_secret
    }

    retention_days = var.retention_days
  }

  timeout = "15m"
}

module "collector" {
  source  = "norca/otel-collector"
  version = "~> 0.1"

  values = {
    export = {
      service = module.openobserve.ingest_service
      secret  = module.openobserve.ingest_secret
    }
    sources = var.collect
  }
}

# ---------------------------------------------------------------------------
# A module of the stack itself
# ---------------------------------------------------------------------------

# Dashboards and alert rules loaded into OpenObserve after every install and
# upgrade. It lives in this stack's directory and is part of its digest.
module "dashboards" {
  source = "./modules/dashboards"

  values = {
    openobserve = {
      service = module.openobserve.service
      secret  = module.openobserve.root_secret
    }
    sets = var.dashboards
  }
}
