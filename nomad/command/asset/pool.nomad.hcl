node_pool "example" {

  description = "Example node pool"

  # meta is optional metadata on the node pool, defined as key-value pairs.
  # The scheduler does not use node pool metadata as part of scheduling.
  meta {
    environment = "prod"
    owner       = "sre"
  }

  # A scheduler configuration specific to the node pool (the scheduler_config
  # block of Nomad) is not supported: a node pool with a scheduler_config
  # block is rejected. The scheduling algorithm and memory oversubscription
  # are set for the whole cluster with 'norca operator scheduler set-config'.
}
