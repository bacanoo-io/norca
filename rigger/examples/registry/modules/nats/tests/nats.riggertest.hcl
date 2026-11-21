test "a single server has no cluster" {
  values = { count = 1 }

  assert {
    condition     = !contains(keys(resource.job.nats.group.nats.network.port), "cluster")
    error_message = "a single server opens no cluster port"
  }

  assert {
    condition     = length(resource.job.nats.group.nats.spread) == 0
    error_message = "a single server is not spread"
  }
}

test "three servers form a cluster over the datacenters" {
  values = { count = 3 }

  assert {
    condition     = resource.job.nats.group.nats.network.port.cluster.static == 6222
    error_message = "the cluster port is the default"
  }

  assert {
    condition     = resource.job.nats.group.nats.spread[0].attribute == runtime("node.datacenter")
    error_message = "a cluster is spread over the datacenters"
  }
}

test "users get their secrets and nothing else" {
  values = {
    users = [{ name = "shop" }, { name = "audit", account = "AUDIT" }]
  }

  assert {
    condition = (
      [for s in resource.job.nats.group.nats.secret_access : s.path] ==
      ["nats/users/shop", "nats/users/audit"]
    )
    error_message = "one secret per user"
  }

  assert {
    condition     = output.accounts == ["APP", "AUDIT"]
    error_message = "the accounts in the order of their first user"
  }
}

test "an even cluster is refused" {
  values       = { count = 4 }
  expect_error = "count must be 1 or an odd number"
}

test "two users of one name are refused" {
  values = {
    users = [{ name = "shop" }, { name = "shop", account = "OTHER" }]
  }
  expect_error = "var.users[1].name is the same as var.users[0].name"
}

test "the bus account must be an account of a user" {
  values = {
    users     = [{ name = "shop" }]
    norca_bus = { servers = ["10.0.0.11:4652"], account = "AUDIT" }
  }
  expect_error = "norca_bus.account must be the account of one of the users"
}
