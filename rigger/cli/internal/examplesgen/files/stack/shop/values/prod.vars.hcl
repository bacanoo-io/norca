# The values of the environment prod:
#   rigger stack plan  -f values/prod.vars.hcl
#   rigger stack apply -f values/prod.vars.hcl
# A value that is a secret is never written here; a sensitive variable of a
# module gets it from the environment (RIGGER_VAR_<name>) or from the
# secret store.

environment = "prod"
web_count   = 3
hostname    = "shop.example.com"
smoke_test  = true

cache_memory      = 512
cache_persistence = true

features = {
  new_checkout = false
}
