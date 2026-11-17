# A module of the stack itself: it lives next to stack.hcl, has no registry
# and no tag; its version is the commit of this repository.

module {
  name        = "shop-api"
  description = "The HTTP API of the shop: reads and writes the shop database, publishes orders to the bus."
  category    = "application"
}
