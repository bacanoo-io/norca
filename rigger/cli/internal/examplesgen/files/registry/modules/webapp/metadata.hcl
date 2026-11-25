# metadata.hcl names the module. Every field of the module block is shown;
# only name is required for a module that is not published, name and
# version for one that is.

module {
  # The name of the module: lower case letters, digits and -, at most 63.
  # It is the directory name in modules/ and the first part of its tags
  # (webapp/v0.1.0), and the default name of a release.
  name = "webapp"

  # Semantic Versioning 2.0.0, without a leading v. Published as the git tag
  # webapp/v<version> by "rigger module publish". A tag is never moved: a
  # change is a new version. Raise the patch version when the rendered
  # resources change, the minor one for a new variable, the major one
  # (the minor one while at 0.x) for a renamed or removed variable or a
  # changed default.
  version = "0.1.0"

  # One sentence: rigger search and the catalog of the web UI show it.
  description = "A stateless HTTP service: health checks, a route through Traefik, settings in a Norca Variable, a session key from the secret store, an optional cache."

  # The home page of the software the module installs.
  url = "https://github.com/traefik/whoami"

  # The license of the software the module installs (an SPDX expression).
  # The image is referenced, never shipped.
  license = "Apache-2.0"

  # One of: application, database, messaging, observability, platform,
  # networking, security, storage, developer-tools, ai, other. A catalog
  # filters on it.
  category = "application"

  # Free words a search finds the module by (rigger search --keyword http).
  keywords = ["web", "http", "example"]

  # The icon of the module: a PNG of exactly 64x64 pixels, at most 32 KiB,
  # in the module directory. "rigger module validate" checks it. Without
  # one the default icon is shown.
  # icon = "icon.png"

  # true makes a library: no resources, only called by other modules
  # (see ../lib/sizing). Default false.
  # library = false

  # What the module needs, checked by a plan against the cluster.
  requires {
    # A version constraint on Norca: "~> 0.4", ">= 0.3.0, < 1.0".
    norca = ">= 0.3.0"

    # Features of the servers the module uses: releases, release-hooks,
    # custom-objects, admission-cel, admission-webhooks,
    # admission-release-signatures, quotas, multiregion-deployments,
    # native-service-catalog, backups, secret-store, mtls, object-webhooks,
    # object-migrations, object-limits.
    features = ["releases", "release-hooks", "secret-store"]

    # Another module that must be installed too: by the same stack, or as a
    # release in the namespace of this one. The label names the module;
    # source and version narrow it down. when names a variable (a path into
    # it): the requirement holds only when that value is true.
    module "cache" {
      # source = "<% .Owner %>/cache"
      version = ">= 0.1.0"
      when    = "cache_enabled"
    }
  }
}

# The tabs of the install form of the web UI. A variable names its tab in
# its ui block. icon is the name of an icon of the UI.
ui {
  tab "general" {
    label = "General"
    icon  = "sliders"
  }

  tab "resources" {
    label = "Resources & Scaling"
    icon  = "cpu"
  }

  tab "networking" {
    label = "Networking"
    icon  = "network"
  }

  tab "security" {
    label = "Security & Secrets"
    icon  = "lock"
  }

  tab "connections" {
    label = "Connections"
  }
}
