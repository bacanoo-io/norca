# Secrets the stack creates in the secret store, once: a later apply keeps
# them unless the block says overwrite = true. "!generate:<name>" is a
# value generated at the first apply. A secret block names one path, in its
# label; the items are the items of the secret.

secret "web-platform/cache" {
  items = {
    password = "!generate:cache-password"
  }
}

secret "web-platform/session" {
  items = {
    key = "!generate:session-key"
  }
}
