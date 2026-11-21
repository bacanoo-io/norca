# The modules come from this same registry, by address and version: a
# published stack never reaches into the repository by path, so it means the
# same wherever it is fetched from. rig.lock.hcl pins what the constraints
# resolved to when this version of the stack was published.

rig {
  required_version = ">= 0.1.0"

  registry "example" {
    source = "github.com/example/rig-registry"
  }
}

module "postgres" {
  source  = "example/postgres"
  version = "~> 0.3"

  values = {
    node      = var.database_node
    databases = var.databases
    settings  = var.database_settings
  }

  atomic = true
}

module "nats" {
  source  = "example/nats"
  version = "~> 1.0"

  values = {
    count = var.bus_servers

    # One user per application, in its own account.
    users = [
      for app in var.bus_applications : {
        name            = app
        account         = upper(app)
        publish_allow   = ["${app}.>", "$JS.API.>"]
        subscribe_allow = ["${app}.>", "_INBOX.>"]
      }
    ]
  }
}
