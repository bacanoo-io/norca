# Written by rigger stack sync: the versions of the modules of the stack and
# where they were found. Kept in version control; not edited.

module "norca/openobserve" {
  version    = "0.2.0"
  constraint = "~> 0.2"
  repository = "github.com/bacanoo-io/norca-registry"
  path       = "modules/openobserve"
  ref        = "openobserve/v0.2.0"
  commit     = "a0b1c2d3e4f5061728394a5b6c7d8e9f00112233"
  digest     = "sha256:3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f3e5a7c9b1d3f5e7a9c1b3d5f7e9a1c3b5d"
  signers    = ["sha256:b1fc32e71de39b2c5f35c03fb9849636cdef8c178ce9e963df659fe665384edd"]
}

module "norca/otel-collector" {
  version    = "0.1.2"
  constraint = "~> 0.1"
  repository = "github.com/bacanoo-io/norca-registry"
  path       = "modules/otel-collector"
  ref        = "otel-collector/v0.1.2"
  commit     = "445566778899aabbccddeeff0011223344556677"
  digest     = "sha256:5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f3e5a7c9b1d3f5e7a9c1b3d5f7e"
  signers    = ["sha256:b1fc32e71de39b2c5f35c03fb9849636cdef8c178ce9e963df659fe665384edd"]
}

module "norca/rustfs" {
  version    = "0.2.1"
  constraint = "~> 0.2"
  repository = "github.com/bacanoo-io/norca-registry"
  path       = "modules/rustfs"
  ref        = "rustfs/v0.2.1"
  commit     = "1f2e3d4c5b6a79880716253443526170f0e1d2c3"
  digest     = "sha256:9d1f3e5c7a9b1d3f5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f"
  signers    = ["sha256:b1fc32e71de39b2c5f35c03fb9849636cdef8c178ce9e963df659fe665384edd"]
}
