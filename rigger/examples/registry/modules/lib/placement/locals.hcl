locals {
  # runtime() names a value the cluster fills in on the node: outside a job
  # block nothing is passed through by itself.
  spread_attribute = {
    datacenter = runtime("node.datacenter")
    node       = runtime("node.unique.id")
  }
}
