# The stack "platform" of region eu: a database and a message bus from a
# registry, and the shop's own modules from this directory, wired together.
#
#   rigger stack sync    resolve versions, verify, write rigger.lock.hcl
#   rigger stack plan    what would change
#   rigger stack apply   make it so, in the order of the dependencies

rigger {
  required_version = ">= 0.1.0"
  name             = "platform" # recorded in every release as its owner
  region           = "eu"
  namespace        = "shop" # default namespace of the releases

  registry "example" {
    source = "github.com/example/rigger-registry"
  }

  # Modules of the registry must be signed with this key. The stack's own
  # modules in ./modules are part of this repository and need no signature.
  trust {
    registry          = "example"
    require_signature = true
    public_keys       = [file("keys/example-registry.pub")]
  }
}

# ---------------------------------------------------------------------------
# From the registry
# ---------------------------------------------------------------------------

module "postgres" {
  source  = "example/postgres"
  version = "~> 0.3"

  values = {
    node = var.node

    databases = [
      { name = "shop", extensions = ["pgcrypto"] },
      { name = "reports", owner = "reporting" },
    ]
  }

  atomic  = true
  timeout = "10m"
}

module "nats" {
  source  = "example/nats"
  version = "~> 1.0"

  values = {
    count = 3

    users = [
      {
        name            = "shop"
        publish_allow   = ["shop.>", "$JS.API.>"]
        subscribe_allow = ["shop.>", "_INBOX.>"]
      },
      {
        name            = "audit"
        account         = "AUDIT"
        subscribe_allow = ["norca.>", "_INBOX.>"]
        publish_allow   = ["$JS.norca-eu.API.CONSUMER.>", "$JS.ACK.>"]
      },
    ]

    # The changes of the region, when the servers listen for leaf nodes.
    norca_bus = length(var.bus_servers) == 0 ? null : {
      servers = var.bus_servers
      account = "AUDIT"
    }
  }
}

# ---------------------------------------------------------------------------
# Built into rigger
# ---------------------------------------------------------------------------

# The base of audit is a module of rigger itself: no registry, no version, no
# signature. Run by the standalone rigger it gets the changes through a leaf
# node (AUDIT.md); once rigger is embedded in the Norca servers it sits on the
# internal bus. What is audited is declared in a resource file of a module
# (modules/shop-api/shop.audit.hcl); subjects, streams, consumers and tokens
# are the business of this module.
module "audit" {
  source = "builtin/audit"
}

# ---------------------------------------------------------------------------
# The stack's own modules
# ---------------------------------------------------------------------------

# The API: installed after the database and the bus are ready, because it
# takes their outputs.
module "shop-api" {
  source = "./modules/shop-api"

  values = {
    count    = var.api_count
    database = module.postgres.databases["shop"]
    bus = {
      service = module.nats.service
      port    = module.nats.client_port
      user    = "shop"
      secret  = "nats/users/shop"
    }
  }
}

# The worker reads the orders the API publishes and writes the reports.
module "shop-worker" {
  source = "./modules/shop-worker"

  values = {
    database = module.postgres.databases["reports"]
    bus      = module.shop-api.bus # the same connection the API uses
    subjects = module.shop-api.order_subjects
  }
}
