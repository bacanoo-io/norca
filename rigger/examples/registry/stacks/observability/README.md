# observability

A stack of this registry built on **the modules of another registry**
(`github.com/bacanoo-io/norca-registry`): this registry ships no module for
the software, only how the modules are put together, sized and wired, plus
one module of its own.

| Release | Module | From |
| --- | --- | --- |
| `rustfs` | `norca/rustfs ~> 0.2` | the external registry |
| `openobserve` | `norca/openobserve ~> 0.2` | the external registry |
| `collector` | `norca/otel-collector ~> 0.1` | the external registry |
| `dashboards` | `./modules/dashboards` | the stack's own, part of its digest |

```
rustfs ──> openobserve ──┬─> collector
                         └─> dashboards
```

What it shows:

- **Two registries, two trust decisions.** Whoever installs the stack trusts
  the key of *this* registry for the stack. The stack names the external
  registry and carries its public key (`keys/norca-registry.pub`), so the
  modules are checked against the right key without the user configuring
  the external registry.
- **The lock pins the external modules** (`rigger.lock.hcl`): version, commit,
  digest and signer of each. A new version of an external module reaches
  users only when this stack is published again.
- **One input, several modules.** `size` chooses the shape of the store and
  of OpenObserve together; a `rule` ties `dashboards` to `collect`.
- **An own module in a published stack:** `modules/dashboards` is fetched
  with the stack and needs no tag or signature of its own.

```shell
rigger stack show  example/observability
rigger stack apply example/observability --version "~> 0.4" -f examples/cluster.vars.hcl
```

Published as the tag `observability/v0.4.0`.

License: the stack is MPL-2.0. The software it installs keeps the licenses
its modules name (RustFS Apache-2.0, OpenObserve AGPL-3.0, OpenTelemetry
Collector Apache-2.0); all of it is referenced, never shipped.
