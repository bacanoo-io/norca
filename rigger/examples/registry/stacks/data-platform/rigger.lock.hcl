# Written by rigger stack sync: the versions of the modules of the stack and
# where they were found. Kept in version control; not edited.

module "example/nats" {
  version    = "1.0.0"
  constraint = "~> 1.0"
  repository = "github.com/example/rigger-registry"
  path       = "modules/nats"
  ref        = "nats/v1.0.0"
  commit     = "dd25e479b41ff63f9b0bc89c72b4305375708aab"
  digest     = "sha256:c8279660cc153e64dd6a277b1863df8c0dfd222bc6ddd4e89202e4f6592c2a60"
}

module "example/postgres" {
  version    = "0.3.0"
  constraint = "~> 0.3"
  repository = "github.com/example/rigger-registry"
  path       = "modules/postgres"
  ref        = "postgres/v0.3.0"
  commit     = "dd25e479b41ff63f9b0bc89c72b4305375708aab"
  digest     = "sha256:b3c7a8489808ab819f0c726f8e37f4213a2fd4828df40bdee16e6d6a515d4a33"
}
