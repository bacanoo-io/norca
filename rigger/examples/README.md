# Rigger examples

Illustrations of the design in `design/rigger-design.md`: how a module, a
registry and a stack look. They are written in the language as it is built,
and the tests keep them so:

- The modules load, validate and render with `rigger module validate` and
  `rigger module render`, and their jobs pass a Norca server's validation.
- The stacks load, and their lock files are lock files as `rigger stack sync`
  writes them. `stack/platform` and `registry/stacks/data-platform` are
  synced and evaluated with their value files by the tests; the modules of
  `registry/stacks/observability` are of a registry the examples do not
  hold, so that stack is read and its lock file checked for its form (see
  "The lock files and the signatures" below).

They change with the design.

| Directory | What it shows |
| --- | --- |
| `module/web/` | a small module on its own: metadata, variables with validation and form hints, one job, outputs, an example value file, a test |
| `registry/` | the layout of a registry repository: three larger modules (`nats`, `postgres`, and `redis`, one server, which began as the built-in pack of norca, design 14.1), a shared child module (`lib/placement`), and two stacks: `stacks/data-platform` built on the registry's own modules, `stacks/observability` built on the modules of another registry |
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

## The lock files and the signatures

The registry of the examples, `github.com/example/rigger-registry`, is no
repository on the network: `registry/` is what it holds. `rigger stack sync`
and `rigger stack validate` therefore can't fetch the modules of an example
stack; the tests stand in. `TestSyncStack_Examples` (in `rigger/`) makes a git
repository of `registry/modules`, one commit with the tag
`<name>/v<version>` of every module on it, the same on every machine, leads
the address of the registry to it, and runs the sync of the engine: the lock
file of a stack must be the file that sync writes, byte for byte.
`TestEngine_ValidateStack_Examples` (in `rigger/cli/norcaapi`) loads each
stack with its lock file and evaluates it with each of its value files, as
`rigger stack validate` does, with the job decoder of Norca.

What follows:

- **The modules of the registry are signed** as `rigger module publish -key`
  and `rigger key sign` sign one: a file `.rigger-signatures/<key ID>.json` in
  the module. The public key is `keys/example-registry.pub` of the stack
  that trusts the registry (`stack/platform`). Nobody has the private key:
  it is thrown away when the modules are signed.
- **An entry of a lock file** is labelled with the source as the stack
  writes it, and records what the sync found:

  ```hcl
  module "example/nats" {
    version    = "1.0.0"
    constraint = "~> 1.0"
    repository = "github.com/example/rigger-registry"
    path       = "modules/nats"
    ref        = "nats/v1.0.0"          # the tag
    commit     = "<the commit of the tag>"
    digest     = "sha256:<the tree digest of the module>"
    signers    = ["sha256:<a key of the trust of the stack that signs it>"]
  }
  ```

  A stack without a `trust` block records no signers. A module of the
  stack's own (`./modules/...`) has no entry, and neither has a child module
  (`lib/placement`, which `nats` calls): the digest of the module that calls
  it covers its files.
- **After a change of a module of `registry/modules`,** or of the modules or
  the constraints of an example stack, the digests of the lock files no
  longer hold and the tests fail. Write the files again, in `rigger/`:

  ```shell
  make examples
  # which is: go test -count=1 -run '^TestSyncStack_Examples$' . -update
  ```

  It signs the modules again when a signature no longer holds, with a new
  key whose public half it writes into the stacks, and writes the lock file
  of every stack it syncs. Commit what it wrote.
- **`registry/stacks/observability` is not synced.** Its modules are of
  `github.com/bacanoo-io/norca-registry`, which the examples do not hold, so
  there is nothing to resolve, take a digest of or verify. Its lock file is
  written by hand in the form of a real one (the tests check that it parses,
  is what a sync would format, and holds the sources of the stack); its
  commits and digests are made up, and `keys/norca-registry.pub` is a key
  made for the example, not a key of that registry.
