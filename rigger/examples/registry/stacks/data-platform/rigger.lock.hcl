# Written by rigger stack sync: the versions of the modules of the stack and
# where they were found (design 8.3). Kept in version control; not edited.

module "example/nats" {
  version    = "1.0.0"
  constraint = "~> 1.0"
  repository = "github.com/example/rigger-registry"
  path       = "modules/nats"
  ref        = "nats/v1.0.0"
  commit     = "f05290a0ffe523331d1fc1b436ee022370a9a59d"
  digest     = "sha256:b22d601c6fb3d774ada451e33a90c224971af32150dabf3ba05d7aed8bc269d0"
}

module "example/postgres" {
  version    = "0.3.0"
  constraint = "~> 0.3"
  repository = "github.com/example/rigger-registry"
  path       = "modules/postgres"
  ref        = "postgres/v0.3.0"
  commit     = "f05290a0ffe523331d1fc1b436ee022370a9a59d"
  digest     = "sha256:b0db7afcf86482a8a9e5e1854987b5e961faafcfc275b7a02ad8cb8b91fbe958"
}
