# Written by `rigger stack sync`, published with the stack.
# The digests are made up for the example.

module "norca/rustfs" {
  version    = "0.2.1"
  constraint = "~> 0.2"
  source     = "github.com/bacanoo-io/norca-registry//modules/rustfs"
  ref        = "rustfs/v0.2.1"
  commit     = "1f2e3d4c5b6a79880716253443526170f0e1d2c3"
  digest     = "sha256:9d1f3e5c7a9b1d3f5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f"
  signers    = ["sha256:8c0e2a4c6e8a0c2e4a6c8e0a2c4e6a8c0e2a4c6e8a0c2e4a6c8e0a2c4e6a8c0e"]
}

module "norca/openobserve" {
  version    = "0.2.0"
  constraint = "~> 0.2"
  source     = "github.com/bacanoo-io/norca-registry//modules/openobserve"
  ref        = "openobserve/v0.2.0"
  commit     = "a0b1c2d3e4f5061728394a5b6c7d8e9f00112233"
  digest     = "sha256:3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f3e5a7c9b1d3f5e7a9c1b3d5f7e9a1c3b5d"
  signers    = ["sha256:8c0e2a4c6e8a0c2e4a6c8e0a2c4e6a8c0e2a4c6e8a0c2e4a6c8e0a2c4e6a8c0e"]
}

module "norca/otel-collector" {
  version    = "0.1.2"
  constraint = "~> 0.1"
  source     = "github.com/bacanoo-io/norca-registry//modules/otel-collector"
  ref        = "otel-collector/v0.1.2"
  commit     = "445566778899aabbccddeeff0011223344556677"
  digest     = "sha256:5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f3e5a7c9b1d3f5e7a9c1b3d5f7e"
  signers    = ["sha256:8c0e2a4c6e8a0c2e4a6c8e0a2c4e6a8c0e2a4c6e8a0c2e4a6c8e0a2c4e6a8c0e"]
}

# ./modules/dashboards is part of the stack: covered by the digest of the
# stack itself, not locked here.
