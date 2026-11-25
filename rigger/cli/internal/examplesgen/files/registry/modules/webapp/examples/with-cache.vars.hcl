# The service with a cache: a release of the module cache in the same
# namespace (metadata.hcl requires one when cache_enabled is true). Before
# the first install, the secrets the job reads:
#
#   norca secret put webapp/session key=-
#   norca secret put cache/password password=-

cache_enabled = true

cache = {
  service = "cache"
  port    = 6379
}

session_secret = "webapp/session"
