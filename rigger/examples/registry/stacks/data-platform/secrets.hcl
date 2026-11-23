# The secret of the stack, created once with a generated value and kept on a
# later apply.

secret "postgres/superuser" {
  items = { password = "!generate:postgres-superuser" }
}

# The modules read one more secret per database (postgres/apps/<name>: user,
# password) and one per application of the bus (nats/users/<name>:
# password). A secret block names one path, written in its label, so a stack
# can't make a secret for each element of a variable. A stack of one's own
# writes a block for each (examples/stack/platform/secrets.hcl); here, where
# the databases and the applications are values of whoever installs the
# stack, they create these secrets before the first apply (README.md).
