# The values of the environment dev:
#   rigger stack validate -f values/dev.vars.hcl
#   rigger stack apply    -f values/dev.vars.hcl

environment = "dev"
web_count   = 1

features = {
  new_checkout = true
}

heartbeat_schedule = "* * * * *"
