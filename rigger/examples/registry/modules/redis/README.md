# redis

One Redis server as a Norca job: a service in the catalog, a password the
server requires, and optionally its data on a host volume. The official
image is referenced, not shipped.

The module is small on purpose. It has **no replication, no Sentinel and no
failover**: with persistence the data is on one host volume of one node,
without it Redis is a cache that starts empty. Run several releases for
several independent servers.

```shell
rigger install . --name cache --namespace apps
rigger install . --name cache --namespace apps --values examples/persistent.vars.hcl
```

In a stack the password is generated and never written down:

```hcl
module "cache" {
  source  = "norca/redis" # "builtin/redis" is the copy the rigger program carries
  version = "~> 0.4"

  values = {
    auth_enabled   = true
    redis_password = "!generate:cache"
  }
}
```

## What it installs

| Resource | When | What |
| --- | --- | --- |
| job `<job_name>` | always | one Redis server (docker), one service `<job_name>` with `service_tags` and TCP checks |
| Variable `nomad/jobs/<job_name>` | `auth_enabled` | the items `user` (`default`) and `password`; the password is a secret item of the release |

Outputs: `service` (the name in the catalog), `port` and `auth_variable`
(the path of the Variable, `null` without authentication).

## Password

With `auth_enabled = true` the server starts with `requirepass`: it refuses
every command of a client that has not sent the password (`AUTH`). The job
reads the password from the Variable by its workload identity when it
starts, into a configuration file in the secrets directory of the task that
only the user of the server can read. The password is not in the job and
not in the release history. A changed password restarts the server. Without
the Variable the task fails instead of starting a server without a password.

`auth_enabled = true` with an empty `redis_password` is refused, and so is a
password with `auth_enabled = false`.

## Data

Without `persistence_enabled` nothing is written to disk (no append-only
file, no snapshot), and a restart loses the data.

With `persistence_enabled = true` the append-only file and the snapshots are
written to `/data`, the client host volume `host_volume`. The server runs
only on nodes that have that volume. The image, started as root, makes the
user `redis` the owner of the directory and runs the server as that user.

## Network

In `bridge` mode (the default) Redis listens on `port` in the network
namespace of the group, and the node maps a port of its own to it. In `host`
mode it listens on the port the node gives. In both, clients find the
address and the port in the service catalog under the name of the service.

## History

The module began as the built-in pack `redis` 0.2.1 of norca, converted on
paper in phase 0 of the design, with the faults of that pack kept. Version
0.4.0 fixes them:

- `mode` and `count` are gone. `mode` changed nothing (every topology ran as
  one standalone server), and a `count` above one gave unrelated servers
  behind one name.
- The job registers a service, with `service_tags`, and passes `extra_env`
  to the container.
- Redis requires the password. Before, the password went into the Variable
  and the server ran without one.
- The Variable is at `nomad/jobs/<job_name>`. Before, it was at
  `nomad/jobs/redis` whatever the name of the job, so a job with another
  name could not read it.
- Persistence has a host volume. Before, the append-only file was written
  into the container and lost with it.

Version 0.4.1 reads the port of the group from `NORCA_PORT_db`, the name
the task has it under. 0.4.0 read `NOMAD_PORT_db`, which no task has: the
`port` line of the configuration was empty and the server did not start.

Version 0.4.2 writes the conditions of the form as conditions
(`show_if = "persistence_enabled"`). 0.4.1 wrote them in the form of the
packs, which the install form did not read: the host volume and the
password were always shown. The resources are the same.
