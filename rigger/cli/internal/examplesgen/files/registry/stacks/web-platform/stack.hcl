# The modules of a published stack come from a registry by address and
# version, also when they are in the same repository: a published stack
# never reaches into the repository by a path, so it means the same
# wherever it is fetched from. rigger.lock.hcl, written by
# "rigger stack sync" and published with the stack, pins what the
# constraints resolved to.

rigger {
  # A version constraint on the rigger program that applies the stack.
  required_version = ">= 0.1.0"

  # The name of the stack: the owner recorded in every release it installs,
  # and the name metadata.hcl publishes it by.
  name = "web-platform"

  # A registry the stack takes modules from: "<% .Owner %>/webapp" is the
  # module modules/webapp of this repository, at the newest tag
  # webapp/v<version> that meets the constraint. The source is the path of
  # the repository on this machine, so the stack works before the
  # repository is pushed anywhere. Once it is, write its git address
  # (github.com/<% .Owner %>/rigger-registry, https://git.example.com/...)
  # and run rigger stack sync again.
  registry "<% .Owner %>" {
    source = <% hcl .RegistryPath %>
  }
}

module "cache" {
  source  = "<% .Owner %>/cache"
  version = "~> 0.1"

  values = {
    memory          = var.cache_memory
    password_secret = "web-platform/cache"
  }
}

# Takes the outputs of the cache, so it is installed after the cache is
# ready.
module "web" {
  source  = "<% .Owner %>/webapp"
  version = "~> 0.1"

  values = {
    count          = var.web_count
    size           = var.web_size
    public         = var.hostname != ""
    hostname       = var.hostname
    session_secret = "web-platform/session"
    cache_enabled  = true
    cache          = module.cache.connection
  }

  # The settings of the release: roll back a failed upgrade, wait up to 10
  # minutes until it is ready, keep 10 revisions.
  atomic      = true
  timeout     = "10m"
  max_history = 10
}
