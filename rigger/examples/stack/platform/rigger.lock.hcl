# Written by `rigger stack sync`. Do not edit; keep it in version control.
# The digests and signatures here are made up for the example.

module "example/postgres" {
  version    = "0.3.0"
  constraint = "~> 0.3"
  source     = "github.com/example/rigger-registry//modules/postgres"
  ref        = "postgres/v0.3.0"
  commit     = "4c1e0b7a9d2f6e3855a1c0d94b7f2e6a8c3d5f10"
  digest     = "sha256:7a1f0c5d2be94e6f8a3b0d1c9e7f4a2b6c8d0e1f3a5b7c9d1e3f5a7b9c1d3e5f"
  signers    = ["sha256:2f8e6c4a0b1d3f5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f3e"]
}

module "example/nats" {
  version    = "1.0.0"
  constraint = "~> 1.0"
  source     = "github.com/example/rigger-registry//modules/nats"
  ref        = "nats/v1.0.0"
  commit     = "9b3d5f7e1a2c4e6f8091a3b5c7d9e1f2a4b6c8d0"
  digest     = "sha256:1c3e5a7b9d1f3e5c7a9b1d3f5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a"
  signers    = ["sha256:2f8e6c4a0b1d3f5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f3e"]

  # Child modules a module calls are locked with it.
  module "lib/placement" {
    version = "1.0.0"
    digest  = "sha256:5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a1c3e5a7b9d1f3e5c7a9b1d3f"
  }
}

# The stack's own modules (./modules/...) are not locked: they are part of
# this repository and change with it.
