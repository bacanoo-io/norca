# rigger stack apply -f values/prod.vars.hcl

node      = "worker-1"
api_count = 4

# The changes of the region reach the NATS of the stack: see AUDIT.md.
bus_servers = ["10.0.0.11:4652", "10.0.0.12:4652", "10.0.0.13:4652"]
