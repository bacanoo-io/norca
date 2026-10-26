<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# 01 – Local development agent

A development agent (`norca agent -dev`: server and client in one process,
state in memory, no ACLs, no TLS) with the new web UI, the native service
catalog with the built-in DNS and the Consul-compatible API, the built-in
secret store and the built-in autoscaler. It is the quickest way to try the
features; everything is gone when the agent stops.

## Prerequisites

- Linux (or macOS) with Docker; the agent's user must be allowed to use
  Docker.
- The `norca` binary built with the web UI (see
  [../README.md](../README.md#getting-the-norca-binary)).
- Optional: `dig` and `curl`.

## Files

| File | |
| --- | --- |
| [dev.hcl](dev.hcl) | Additions to the dev agent: autoscaler, DNS, Consul-compatible API, zone |
| [test-app.hcl](test-app.hcl) | Variables of the [test-app pack](../packs/test-app/README.md) for a dev agent |

## Steps

Run the commands from this directory.

### 1. Start the agent

```shell
norca agent -dev -config dev.hcl
```

In a second terminal, the CLI talks to `http://127.0.0.1:4646` by default.
Open the web UI at <http://127.0.0.1:4646/ui/>.

### 2. Put a secret

```shell
norca secret put -description "Database login of test-app" test-app/db username=app password=-
# type a password, then Ctrl-D
norca secret list
norca secret info test-app/db        # metadata and versions, never the value
```

### 3. Run the test application

```shell
norca pack run ../packs/test-app -f test-app.hcl
norca pack status test-app
norca job status test-app
```

`network_mode = "host"` in `test-app.hcl` uses Docker port mapping, so no
CNI plugins and no root agent are needed. For bridge networking run the
agent as root with the CNI plugins in `/opt/cni/bin` and drop that line.

### 4. Look at it

```shell
# Native catalog with the health of every instance and the check roles
norca service info -verbose test-app

# Built-in DNS (healthy instances only)
dig @127.0.0.1 -p 8600 test-app.service.nomad SRV
dig @127.0.0.1 -p 8600 test-app.service.consul SRV   # consul_compat names

# Consul-compatible API, as Traefik or Prometheus would read it
curl http://127.0.0.1:8500/v1/catalog/services
curl 'http://127.0.0.1:8500/v1/health/service/test-app?passing'

# The page shows the version of the secret read with a Vault-style template
curl http://<address:port from service info>/

# Built-in autoscaler: the typed policy and its decisions
norca scaling policy list
norca job scaling-events test-app
```

### 5. Rotate the secret

```shell
norca secret put test-app/db username=app password=-
norca secret info test-app/db
curl http://<address:port>/            # "version 2" once the template re-rendered

# Audit trail of the reads (one JSON event per read; stop with Ctrl-C)
norca operator api '/v1/event/stream?topic=SecretAccess'
```

The `secret` block of the job delivers the new version into
`secrets/db.env` and the environment (with `change_mode = "signal"`, nginx
gets `SIGHUP`); the Vault-style template (`{{ with secret "kv/data/test-app/db" }}`)
is served by the built-in store because the agent has no Vault integration.

### 6. External service and DNS alias (optional)

```shell
norca service register -alias db.example.com legacy-db
dig @127.0.0.1 -p 8600 legacy-db.service.nomad    # CNAME db.example.com
norca service deregister legacy-db
```

### 7. Teardown

```shell
norca pack uninstall test-app
```

Then stop the agent with Ctrl-C: the dev agent keeps nothing.

## What to look at in the web UI

- **Dashboard** (landing page): jobs, allocations, clients, live updates
  over the event stream.
- **Jobs → test-app**: allocations, the service with its startup, readiness
  and liveness checks, the **Scaling** tab with the typed policy, versions
  and the job definition (HCL/JSON).
- **Services**: the catalog with the health of every instance.
- **Scaling**: the policies of the built-in autoscaler and their events.
- **Packages**: the `test-app` release and its revisions.
- **Topology** and **Clients**: the node with its zone `dev`.

The built-in secret store has no page in the UI yet: use `norca secret ...`.
The task events of the allocation show which provider delivered each secret,
never the value.
