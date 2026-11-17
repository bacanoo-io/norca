# rig stack apply -f values/dev.vars.hcl
# No change data capture: the NATS of the stack is not attached to the bus.

node      = "dev-1"
api_count = 1
