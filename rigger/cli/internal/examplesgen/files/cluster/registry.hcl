# The registry of this project as a registry of a cluster: an object of the
# built-in kind rigger.norca.io/Registry. The servers install modules from
# it (rigger install webapp --registry <% .Owner %> --server) and the web UI
# shows its modules and stacks in its catalog.
#
#   norca object apply -dry-run cluster/registry.hcl   check it, write nothing
#   norca object apply cluster/registry.hcl            create or update it
#   rigger registry sync <% .Owner %>                      fetch its index now
#   rigger registry status <% .Owner %>
#   rigger registry search <% .Owner %> webapp
#
# A registry is synced only when someone asks for it: searches and installs
# read it at the commit of its last good sync. The object needs the write
# capability on the kind (namespace "*" { object "rigger.norca.io/Registry"
# { capabilities = ["read", "write"] } }).

kind = "rigger.norca.io/Registry"
name = "<% .Owner %>" # cluster-scoped: no namespace

spec {
  # git: a registry repository (modules/<name>, stacks/<name>, tags
  # <name>/v<version>). global is only the registry named global.
  type = "git"

  # Where the servers fetch it. This path is the repository on this
  # machine, which the servers read only when they run here (a dev agent);
  # otherwise push the repository and write its address:
  # "https://git.example.com/<% .Owner %>/registry", "github.com/<% .Owner %>/rigger-registry",
  # "ssh://git@git.example.com/<% .Owner %>/registry". An address carries no
  # credentials.
  source = <% hcl .RegistryPath %>

  # The public keys the versions must be signed with, checked at every
  # install. file() reads a file relative to this one.
  trustedKeys = [file("../keys/registry.pub")]

  # How the web UI shows the registry: catalog (every module and stack) or
  # selected (only those of the selected blocks). A search and an install
  # find everything either way.
  display = "catalog"

  # selected {
  #   kind = "module"
  #   name = "webapp"
  # }

  # The credentials of a private source: a Norca Variable with username and
  # password (or token) for https, ssh_private_key and known_hosts for ssh.
  # Whoever writes the registry or installs from it must be able to read it.
  # authSecret {
  #   namespace = "default"
  #   path      = "registries/<% .Owner %>"
  # }
}
