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
- **The lock pins the external modules** (`rigger.lock.hcl`): version,
  repository, tag, commit, digest and signer of each. A new version of an
  external module reaches users only when this stack is published again.
- **One input, several modules.** `size` chooses the shape of the store and
  of OpenObserve together; a `rule` ties `dashboards` to `collect`.
- **An own module in a published stack:** `modules/dashboards` is fetched
  with the stack and needs no tag or signature of its own.

```shell
rigger stack plan  github.com/example/rigger-registry//stacks/observability -version "~> 0.4" -f examples/cluster.vars.hcl
rigger stack apply github.com/example/rigger-registry//stacks/observability -version "~> 0.4" -f examples/cluster.vars.hcl
```

Published as the tag `observability/v0.4.0`.

**What is made up here.** The modules `rustfs`, `openobserve` and
`otel-collector` are not modules the examples hold, and
`github.com/bacanoo-io/norca-registry` publishes no such versions. So,
unlike the lock files of `data-platform` and `../../../stack/platform`,
which a sync wrote, this lock file is written by hand: it has the form of a
real one (it is read and written again by the lock code in the tests), and
its versions, commits and digests are invented. `keys/norca-registry.pub` is
a real key made for the example, whose private half was thrown away; the
signers of the lock file name it. The stack is read by the tests, and its
example values are checked against its variables; it can't be planned.

License: the stack is MPL-2.0. The software it installs keeps the licenses
its modules name (RustFS Apache-2.0, OpenObserve AGPL-3.0, OpenTelemetry
Collector Apache-2.0); all of it is referenced, never shipped.
