# Rigger examples

Illustrations of the design in `design/rigger-design.md`: how a module, a
registry and a stack look. The modules load, validate and render with
`rigger module validate` and `rigger module render`, and their jobs pass a Norca
server's validation; the stacks wait for phase 2. They change with the
design.

| Directory | What it shows |
| --- | --- |
| `module/web/` | a small module on its own: metadata, variables with validation and form hints, one job, outputs, an example value file, a test |
| `registry/` | the layout of a registry repository: three larger modules (`nats`, `postgres`, and `redis`, the built-in pack of norca converted on paper with its faults kept, design 14.1), a shared child module (`lib/placement`), and two stacks: `stacks/data-platform` built on the registry's own modules, `stacks/observability` built on the modules of another registry |
| `stack/platform/` | a stack built from two registry modules (`postgres`, `nats`) and two modules of its own (`modules/shop-api`, `shop-worker`), wired through outputs, with values per environment, trust and a lock file |

What to look for:

- **No templates.** Conditions and loops are `dynamic` blocks, `for`
  expressions and `enabled`; configuration files are built from data with an
  encoder (`natsconf`, `jsonencode`); longer texts use the template syntax of
  HCL itself.
- **Install time and run time.** `var.*`, `local.*`, `module.*`, `rigger.*` are
  resolved by Rigger. Everything else inside a job (`${NORCA_PORT_client}`,
  `${attr.kernel.name}`, the `{{ }}` of a task template) belongs to the
  cluster and passes through; in the data of a task template a runtime
  reference becomes `{{ env "..." }}`.
- **Checks before anything runs:** `validation` in variables, `rule` across
  variables, `check` over evaluated values.
- **Secrets are never values.** A variable with a `secret` block names where
  the secret store holds the secret; the task reads it at run time.
- **Dependencies:** `requires` in `metadata.hcl`, outputs of one module as
  values of another in the stack.

## Files are named after what they hold

| File | Holds |
| --- | --- |
| `metadata.hcl` | the module (`module` block) or the published stack (`stack` block): name, version, license, `requires`, form tabs |
| `variables.hcl` | `variable` and `rule` blocks |
| `locals.hcl` | `locals` and `check` blocks |
| `modules.hcl` | calls of child modules |
| `outputs.hcl` | `output` blocks |
| `<name>.norca.hcl` | one job |
| `<name>.acl.hcl` | one ACL policy |
| `<name>.var.hcl` | one Norca Variable |
| `<name>.kind.hcl`, `.admission.hcl`, `.namespace.hcl`, `.quota.hcl`, `.external.hcl`, `.object.hcl` | one resource of that kind |
| `files/*.tftpl` | a text in the template syntax of HCL, read with `templatefile()`; a template can't call `templatefile()` itself |
| `files/*` (other) | a plain file, read with `file()` |
| `examples/*.vars.hcl`, `values/*.vars.hcl` | values of variables |
| `tests/*.riggertest.hcl` | tests |
| `stack.hcl` | a stack: the `rigger` block and the modules it installs |
| `secrets.hcl` | the secrets a stack creates once |
| `rigger.lock.hcl` | the lock of a stack |

The resource extensions are the ones packs use today, without `.tpl`. A
block in a file of another name is an error, so a directory listing says
what a module installs.

Details of the language the examples rely on (design sections 4.2, 5, 6):

- `runtime()`, `runtime_env()`, `secret_ref()`, `service_address()` name
  values the cluster resolves on the node;
- a stack has modules of its own next to it (`./modules/...`), without a
  registry, a version or a signature;
- tests are `tests/*.riggertest.hcl` with `test` blocks, `values`, `assert` and
  `expect_error`.
