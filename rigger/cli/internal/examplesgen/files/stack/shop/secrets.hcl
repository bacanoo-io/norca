# Secrets the stack creates in the secret store of the cluster at the first
# apply, and keeps on a later one (overwrite = true writes them again).
# "!generate:<name>" is a generated value; a fixed value is never written
# here, because this file is in version control.

secret "shop/cache" {
  items = {
    password = "!generate:cache-password"
  }
}

secret "shop/web-session" {
  items = {
    key = "!generate:web-session"
  }

  # overwrite = false
}
