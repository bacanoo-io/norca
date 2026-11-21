# rigger stack apply example/observability --version "~> 0.4" -f cluster.vars.hcl

size           = "cluster"
retention_days = 30
collect        = ["host", "norca", "nats", "postgres"]
dashboards     = ["cluster", "nodes", "nats", "postgres"]
