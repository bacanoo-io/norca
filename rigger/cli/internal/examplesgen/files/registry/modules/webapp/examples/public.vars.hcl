# Values of the module: a file of variable = value lines, given with
# -f (--values). "rigger module validate" renders the module with every
# file of examples/ and checks the jobs it gives.
#
#   rigger module render modules/webapp -f modules/webapp/examples/public.vars.hcl --release shop
#   rigger install modules/webapp --name shop -f modules/webapp/examples/public.vars.hcl

count = 3
size  = "medium"

public   = true
hostname = "shop.example.com"

log_level = "debug"

features = {
  new_checkout = true
  dark_mode    = false
}

env = {
  TZ = "UTC"
}

smoke_test = true
