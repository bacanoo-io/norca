# A stack of one's own: three releases from two kinds of modules, wired
# through their outputs. It has no metadata.hcl, so it is never published:
# it lives in the repository of whoever runs it.
#
#   rigger stack sync                            resolve versions, verify signatures, write rigger.lock.hcl
#   rigger stack validate -f values/dev.vars.hcl evaluate it, without a cluster
#   rigger stack plan  -f values/prod.vars.hcl   what an apply would change
#   rigger stack apply -f values/prod.vars.hcl   install or upgrade, in the order of the dependencies

rigger {
  # A version constraint on the rigger program.
  required_version = ">= 0.1.0"

  # The name of the stack: recorded in every release as its owner, so that
  # status, drift and --prune know what the stack installed.
  name = "shop"

  # The region the stack is applied in (default: global) and the namespace
  # of a release that names none (default: default). A module block may
  # set namespace = "..." for its release.
  # region  = "global"
  namespace = "shop"

  # The registry of this project, by the path of its repository on this
  # machine: "<% .Owner %>/webapp" is modules/webapp there, at the newest tag
  # webapp/v<version> that meets the version constraint. Once the registry
  # is pushed to a git host, write its address instead
  # (github.com/<% .Owner %>/rigger-registry, https://git.example.com/<% .Owner %>/registry,
  # ssh://git@git.example.com/<% .Owner %>/registry) and run rigger stack sync.
  # Credentials of a private host are never written here: rigger login, or
  # RIGGER_GIT_USERNAME and RIGGER_GIT_PASSWORD.
  registry "<% .Owner %>" {
    source = <% hcl .RegistryPath %>
  }

  # Modules of the registry must be signed with this key: the public half of
  # the key the registry signs with (keys/README.md). A module of the stack's
  # own (./modules/...) is part of this repository and needs no signature.
  trust {
    registry          = "<% .Owner %>"
    require_signature = true
    public_keys       = [file("keys/registry.pub")]
  }
}

# ---------------------------------------------------------------------------
# From the registry
# ---------------------------------------------------------------------------

# The label is the name of the release: lower case letters, digits and -.
module "cache" {
  source  = "<% .Owner %>/cache"
  version = "~> 0.1" # >= 0.1.0, < 0.2.0; "0.1.0" pins one version

  values = {
    memory          = var.cache_memory
    persistence     = var.cache_persistence
    password_secret = "shop/cache" # created once by secrets.hcl
  }
}

# Takes outputs of the cache, so it is installed after the cache is ready.
module "web" {
  source  = "<% .Owner %>/webapp"
  version = "~> 0.1"

  values = {
    image          = var.web_image
    count          = var.web_count
    size           = local.web_size
    public         = var.hostname != ""
    hostname       = var.hostname
    session_secret = "shop/web-session" # created once by secrets.hcl
    cache_enabled  = true
    cache          = module.cache.connection # an output of another release
    features       = var.features
    smoke_test     = var.smoke_test
  }

  # The settings of the release: roll back a failed upgrade (atomic), wait
  # until it is ready (wait, default true) at most timeout, keep max_history
  # revisions.
  atomic      = true
  wait        = true
  timeout     = "10m"
  max_history = 10
}

# The same module from the global registry: without the registry block
# named "<% .Owner %>" above, "<% .Owner %>/webapp" names the module webapp of the
# namespace <% .Owner %> in the global registry (github.com/bacanoo-io/norca-global-registry,
# or the index RIGGER_GLOBAL_REGISTRY names: ../../index). The official
# modules of Norca are the namespace norca there:
#
#   module "redis" {
#     source  = "norca/redis"
#     version = "~> 0.4"
#   }
#
# A published stack can be taken as a part; its releases are named
# <part>-<release>, its outputs read as stack.<part>.<output>:
#
#   stack "platform" {
#     source  = "<% .Owner %>/web-platform"
#     version = "~> 1.0"
#     values  = { hostname = var.hostname }
#   }

# ---------------------------------------------------------------------------
# The stack's own modules
# ---------------------------------------------------------------------------

# A module in ./modules of this directory: no registry, no tag, no
# signature, not in the lock file; versioned with this repository.
module "heartbeat" {
  source = "./modules/heartbeat"

  # enabled = false keeps the entry without installing it.
  enabled = var.heartbeat_schedule != ""

  values = {
    schedule = var.heartbeat_schedule
    target   = module.web.service
    port     = module.web.port
    cache    = module.cache.connection
  }

  # The order follows from the references above; depends_on adds one that
  # no value shows.
  depends_on = [module.web]
}
