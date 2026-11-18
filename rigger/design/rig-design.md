# Rig: the Package Manager of Norca (design draft)

Status: draft for discussion, 2026-11-15; phase 0 done 2026-11-18 (section 15:
the library's ports and constructor, the CLI host, the spikes of the job
decoder and of the release API, redis and nats converted on paper); phase 1
under way: modules load, validate and evaluate (`rig module validate`,
`rig module render`). `examples/` shows a module, a registry with three larger
modules and a stack in this language; every module of it renders, and a
Norca server accepts the rendered jobs (`/v1/validate/job`). It starts from the
maintainer's first sketch (`rig.md`, not in this repository) and from an inventory of
what exists today: the pack manager in norca (about 32k lines, plus about
13k on the server for registries and releases), the 14 packs of norca-registry,
and the pack handling of norca-deployer.

Rules of this work, set by the maintainer:

- norca, norca-deployer and norca-registry are **not changed** until Rig is
  ready. Rig is built in its own repositories and works against the Norca API
  as it is today.
- **Rig is a new development, written from the start.** No code is lifted
  from the pack manager. Its sound ideas (logic, the signing scheme, some
  structures) are built again, which leaves us free to shape them; its faulty
  foundations are thought again.
- **Nothing is taken over from code that is not open source** (2026-11-18):
  no implementation is copied or closely rewritten from code under the
  Business Source License (Terraform, and Nomad, Vault and Consul in their
  later versions) or any other license that is not open source. Names and
  behaviour may be like those of other tools; the implementation is Rig's,
  or an open-source dependency's. `make check-licenses` checks the
  dependencies and the files of the repository.
- **The library has an interface that Norca can take in** (section 12):
  the server embeds Rig later through it, without Rig knowing the server.
- Go templates go. The result is a complete package manager, not a renderer.
- Rig respects the working principles of norca-deployer.
- Everything can be extended.
- The design stays in this repository.

## 1. Goals and non-goals

Goals:

1. **Typed HCL2 instead of text templates.** A package is HCL that is parsed
   once, checked against schemas and evaluated with typed values. An error
   names a file, a line and a block.
2. **A complete package manager**: packages with versions, dependencies,
   registries, a lock file, signatures and trust, install, upgrade, rollback,
   uninstall, drift, an air-gapped path.
3. **One library, two ways to run it.** The engine is a Go library with a
   public interface (section 12). It runs as an external application (the
   `rig` program, with local data of its own) and embedded in the Norca
   server (server-side evaluation, the install form of the UI). Both are
   built; which stay is decided at the end.
4. **Declarative and idempotent**, as the deployer is: a description of the
   wanted state, validation before any change, an `apply` that changes only
   what differs, a `plan` that shows what would change.

Non-goals (for now):

- A general infrastructure tool. Rig manages what a Norca cluster holds
  (section 5), not machines, clouds or DNS.
- Compatibility with packs. Nothing is released; packs are converted by hand,
  with a helper for the mechanical part (section 14).
- A state file. The cluster is the state (section 9).

## 2. What we keep as ideas, what we think again

### 2.1 Ideas kept, built again

These proved themselves in the pack manager, the registry and the deployer.
Rig implements them anew; the column says what Rig does differently.

| Idea kept | Known from | In Rig |
| --- | --- | --- |
| Variable schema: `type`, `default`, `description`, `validation { pattern, min, max, min_items, unique, ... }`, `ui { tab, group, widget, show_if, ... }`, `secret { path, item }`, cross-variable `rule` blocks | the pack manager | validation of list items by key added (section 6.2), so no check is left outside the schema |
| The install form: a JSON Schema synthesized from the variables | the pack manager and the UI | hints are explicit; no guessing of tabs, widgets and categories from names |
| Releases on the server: revisions, history, rollback, hooks, drift check, ownership, multi-region, the fixed order of resource kinds | the Norca server | used as they are through the API; Rig adds release dependencies on its side |
| Resource kinds a package may hold: namespace, quota, ACL policy (bound to a job), variable, object kind, admission policy, external service, object, job | the pack manager, the operator packs of the registry | typed blocks instead of file suffixes |
| Signing: ECDSA P-256 keys in the cosign format, key ID = SHA-256 of the public key, trust policy `require_signature` + `public_keys`, re-verification of cached content | the pack manager | what is signed changes (section 8.1); existing keys stay usable |
| Tree digest: SHA-256 over the sorted `<file hash>  <path>` lines | the pack manager | becomes *the* identity of a package version |
| Lock file with digests, `--lock=verify\|require\|update` | the pack manager | one format for modules and stacks |
| Air-gapped bundle: one reproducible archive of packages | the pack manager | same idea, new content |
| Registry rules: per-package semver, tags never moved, third-party software referenced and never shipped, license named, examples rendered in CI, LF line ends | norca-registry | per-package versions become addressable (section 7) |
| Server-side registry object with sync, staged swap, push notice | the Norca server | unchanged for now; a Rig registry kind is added when the server embeds Rig |
| Value forms: `!generate`, `!nkey-seed`, `!enc:`, `@file` | norca-deployer | Rig understands them in stack values (section 6.4) |

Because nothing is copied, the formats are free to change where it helps
(file names, the lock format, the bundle layout, the statement that is
signed); only the server's release API and the key format are given.

### 2.2 Thought again

| Faulty foundation today | What goes wrong | In Rig |
| --- | --- | --- |
| Go `text/template` with `[[ ]]` over HCL text | output is checked only after rendering; `<no value>` stripped by string replace; `indent N` passed as an argument; `toStringList` to print a list; `fmt` can't parse most templates; errors found by parsing error text | no templates: HCL expressions, `dynamic` blocks, typed functions (section 4) |
| Two template languages in one file (`[[ ]]` at install time, `{{ }}` at run time) with hand-written escaping | the nats pack escapes `\` and `"` by `replaceAll` inside a runtime template | install-time values are HCL; runtime templates stay the task's own business and get values through typed functions (`jsonencode`, `quote`) |
| Checks written as template `fail` calls | 9 in consul, more in nats, forgejo, mtls-gateway; found only at render | every check is a `validation`, a `rule` or a `check` block, run before anything is evaluated |
| Variables travel as strings (form JSON → `--var` strings → parsed again) | types lost and guessed back | cty values from the form to the engine |
| Job meta injected by parsing the rendered text again, silently skipped on a parse failure | `pack.*` meta lost without an error | Rig owns the job value and sets the meta on it |
| A signature covers the OCI manifest | git and dir packages have nothing a signature binds to; a client-rendered release claims a digest the server can't check | the signature covers the tree digest of the package, whatever carried it; the release records that digest |
| The registry is versioned as a whole (`v0.2.0` of everything); the default ref is a moving `latest` | a package version can't be asked for; changing the ref redeploys nothing in the deployer | every module version has its own tag and an index entry; a stack locks digests |
| Dependencies are sub-packs rendered into the same release | forgejo needs rustfs, otel needs openobserve: only in READMEs; no order between releases | `requires { module ... }` and stack-level dependencies with order and wait (section 6.5) |
| Built-in packs as Go string maps (1356 lines) | not testable as packages | ordinary module directories embedded with `go:embed` |
| "Unchanged" decided by marker files next to the deployer | no drift check, removing an entry uninstalls nothing, markers hold resolved secrets | the release on the server records what was applied; Rig compares digests with it (section 9) |
| Secret redaction by name patterns (`password`, `token`) | a secret under another name leaks into history | `sensitive = true` and `secret {}` are part of the type; sensitive values never enter a revision |
| Left-overs: parser v1, `nomad/packs` cache path, `integration {}`, upstream TODOs | dead weight | not carried over |

## 3. Concepts

| Term | Meaning |
| --- | --- |
| **Rig** | the tool and the library |
| **Module** | a package: one service or building block with variables, resources and outputs; has a version, a digest and an icon |
| **Release** | a module installed in a cluster under a name, with revisions (the server's release object of today) |
| **Stack** | a description of which modules run in a region with which values: the unit of `plan` and `apply` |
| **Registry** | a place that holds versions of **modules and stacks**: a git repository, an OCI registry, a directory, a bundle |
| **Global registry** | the one public registry, on the model of the OpenTofu registry: the modules and stacks of all users under namespaces (section 7.3) |

A module instance in a stack is one release. A stack is not a release of its
own: it is the plan of several releases, as the `packs.deploy` list of the
deployer is today. A stack lives in the repository of its user, or is
published in a registry like a module (section 7.5).

Decided by the maintainer (2026-11-15): the package is a **module**, the tool
is **rig**. In this document *module* always means a package of Rig; the Go
code that Norca takes in is called the *library* (section 12), never a
module.

## 4. The language

### 4.1 Principles

- A module is a directory of `.hcl` files, parsed with `hashicorp/hcl/v2`.
  The order of files and blocks has no meaning.
- **A file is named after what it holds** (section 4.5): a listing of the
  directory says what the module installs. Names and extensions that packs
  have today keep their meaning; nothing is renamed for its own sake.
- Every block has a schema. An unknown block or attribute is an error (the
  rule of the deployer plan, and of the ACL rules since 2026-11-15).
- Values are typed (`cty`): string, number, bool, list, map, object, with
  optional attributes and defaults.
- No text is generated and parsed again anywhere.
- `&&` and `||` evaluate both sides in the hcl of Norca (the version Rig
  must keep, section 12.2): `x == null || x.a == ""` fails on a null `x`.
  A guard is the conditional operator, `x == null ? true : x.a == ""`
  (found while checking the example values, phase 1).

### 4.2 What is evaluated when

A job specification names things that only exist on a node at run time
(`${attr.kernel.name}`, `${node.class}`, `${NORCA_PORT_http}`, `${env "X"}` in
a task template). Rig evaluates these namespaces at install time:

| Namespace | Content |
| --- | --- |
| `var.<name>` | the variables of the module |
| `local.<name>` | `locals` of the module |
| `module.<name>.<output>` | outputs of a child module, or of another module of the stack |
| `rig.*` | facts of the install: `rig.release`, `rig.namespace`, `rig.region`, `rig.module.name`, `rig.module.version` |
| `each.*`, `count.index` | inside `for_each` / `count` |

Any other reference inside a string of a `job` is a runtime reference and is
passed through untouched, as jobspec2 does today. Nothing is written `$${}`
because of Rig. A runtime name that is misspelled as an install-time one
(`${vars.x}`, `local` for `locals`: one letter away) is an error with a hint,
and so is `each.*` or `count.*` outside a resource with `for_each` or
`count`.

Outside a `job` block (in `locals`, in a child module that only computes
values) nothing passes through by itself, so runtime values are named with
functions. They return the text the cluster resolves later, and Rig knows
that the value is not known at install time:

| Function | Gives |
| --- | --- |
| `runtime("node.datacenter")` | a node attribute or other job interpolation |
| `runtime_env("NORCA_ALLOC_DIR")` | an environment variable of the task |
| `secret_ref(path, item, { quote, trim })` | inside a task template: the item of a secret, quoted for where it lands (`"conf"`, `"sql"`, `false`), failing the template when the item is missing |
| `variable_ref(path, item, { quote, trim })` | the same for an item of a Norca Variable (`nomadVar` of today's packs: the nats pack reads its TLS material and its token there; proposed by the conversion of section 14) |
| `service_address(name, { scheme, port })`, `service_addresses(name, { scheme, exclude_self })` | inside a task template: addresses from the native catalog; `port = false` gives the host alone, `service_addresses` is a list `concat()` can put known items before |

Where a runtime value lands decides how it is written, and only the host
knows a job's schema, so these functions (and a runtime reference written
in a job) give a placeholder (package `runref`) that the job decoder of the
host resolves: in an attribute of a job `${NORCA_ALLOC_DIR}`, which the
client interpolates; in the data of a task template `{{ env
"NORCA_ALLOC_DIR" }}` (the template sees the node attributes there too,
`{{ env "node.datacenter" }}`), the `norcaSecret` / `nomadVar` lookup, or the
`nomadService` range. A secret, a Variable item or a service address
outside the data of a task template is an error. Without `quote` the place
decides: none in a task template, the quoting of the configuration in an
encoder (a whole value quoted, a part of a string escaped). Outside a job
(another kind, an output) only `runtime()` and `runtime_env()` resolve, as
`${...}`.

These replace the hand-written `{{ with nomadSecret ... }}` and
`{{ range nomadService ... }}` of today's packs with their escaping. A task
template may still be written by hand; the functions are for values that end
up inside generated configuration (see `examples/registry/modules/nats`).

### 4.3 What replaces what

| Template construct | In Rig |
| --- | --- |
| `[[ if $v.tls_enabled ]] ... [[ end ]]` around a block | `dynamic "x" { for_each = var.tls_enabled ? [1] : [] ... }`, or `enabled = var.tls_enabled` on a resource |
| `[[ range $v.users ]] ... [[ end ]]` | `dynamic` blocks, `for` expressions |
| `[[ template "nats.port" (dict ...) ]]` helpers | `locals`, child modules that only compute outputs, built-in functions |
| a config file built by text with `indent 10` | a value built from data and encoded: `jsonencode`, `yamlencode`, `tomlencode`, `natsconf` ... (encoders are extensible, section 11) |
| a longer text with conditions | the template syntax of HCL itself (`${}`, `%{ if }`, `%{ for }`) in a heredoc or in a file of the module read with `templatefile("files/x.tftpl", { ... })`: typed, parsed with the module, errors with a position. Allowed by decision (question 2); a template file sees only the values it is given |
| `[[ fail "..." ]]` | `validation`, `rule`, `check` |
| `fileContents` relative to the working directory | `file("files/x")`, relative to the module, inside it only |
| `nomadNamespace`, `nomadRegions` (live API calls from a template) | `rig.namespace`, `rig.region`; lookups of the cluster are `data` blocks, which the first version does not have (decided, question 6) |

### 4.4 A module, in short

```hcl
# metadata.hcl
module {
  name        = "nats"
  version     = "1.0.0"
  description = "A NATS cluster per region for the applications."
  url         = "https://nats.io"
  license     = "Apache-2.0"        # of what it installs
  category    = "messaging"
  keywords    = ["nats", "jetstream"]

  requires {
    norca    = ">= 0.3.0"
    features = ["releases"]
  }
}
```

```hcl
# variables.hcl
variable "count" {
  type        = number
  default     = 3
  description = "Servers in the region (odd, at least 3 for JetStream)."

  validation {
    min     = 1
    integer = true
  }

  ui {
    tab    = "general"
    widget = "slider"
  }
}

variable "users" {
  type = list(object({
    name    = string
    account = optional(string, "APP")
  }))
  default = []
  validation { unique_by = "name" }      # was a template `fail`
}

rule "users_need_userpass" {
  condition     = length(var.users) == 0 || var.auth_mode == "userpass"
  error_message = "users needs auth_mode = \"userpass\"."
}
```

```hcl
# locals.hcl
locals {
  job       = coalesce(var.job_name, rig.release)
  clustered = var.count > 1
}
```

```hcl
# nats.norca.hcl
job "nats" {
  id          = local.job
  name        = local.job
  datacenters = var.datacenters

  group "nats" {
    count = var.count

    network {
      port "client" { static = var.client_port }

      dynamic "port" {
        for_each = local.clustered ? ["cluster"] : []
        labels   = [port.value]
        content { static = var.cluster_port }
      }
    }

    task "nats" {
      driver = var.driver
      config { image = image_ref(var.image, var.image_digest) }

      template {
        destination = "secrets/nats.conf"
        # data, not text: the encoder writes the NATS configuration
        data = natsconf({
          server_name = "${NORCA_ALLOC_NAME}"    # a runtime reference, passed through (4.2)
          jetstream   = var.jetstream_enabled ? { max_file_store = var.jetstream_max_file } : null
        })
      }
    }
  }
}

```

```hcl
# nats.acl.hcl
policy "nats" {
  enabled = var.acl_policy_enabled
  rules   = file("files/policy.hcl")
  job_acl { job_id = local.job }
}
```

```hcl
# outputs.hcl
output "client_service" { value = local.job }
output "client_port"    { value = var.client_port }
```

The `job` block is the job specification of Norca with its own schema; Rig
does not invent a second one. The core resolves the install-time values and
the host decodes the block with Norca's own job decoder (section 12.2); that
the decoder can take its values from outside is a spike of phase 0
(question 4).

### 4.5 File names

| File | Holds | Today |
| --- | --- | --- |
| `metadata.hcl` | the `module` block (name, version, license, `requires`), form tabs; in a published stack the `stack` block | `metadata.hcl` |
| `variables.hcl` | `variable`, `rule` | `variables.hcl` |
| `locals.hcl` | `locals`, `check` | template helpers |
| `modules.hcl` | calls of child modules | `dependency` blocks |
| `outputs.hcl` | `output` | `outputs.tpl` |
| `<name>.norca.hcl` | one job | `<name>.nomad.tpl` |
| `<name>.namespace.hcl` | one namespace | `.namespace.hcl.tpl` |
| `<name>.quota.hcl` | one quota | `.quota.hcl.tpl` |
| `<name>.acl.hcl` | one ACL policy | `.acl.hcl.tpl` |
| `<name>.var.hcl` | one Norca Variable | `.var.hcl.tpl` |
| `<name>.kind.hcl` | one object kind | `.kind.hcl.tpl` |
| `<name>.admission.hcl` | one admission policy | `.admission.hcl.tpl` |
| `<name>.external.hcl` | one external service | `.external.hcl.tpl` |
| `<name>.object.hcl` | one object | `.object.hcl.tpl` |
| `<name>.audit.hcl` | one audit declaration: what is recorded, how long, who may read (section 12.8) | new |
| `files/*.tftpl` | a text in the template syntax of HCL, for `templatefile()` | |
| `files/*` | plain files, for `file()` | |
| `*.vars.hcl` | values of variables (`examples/`, a stack's `values/`) | var files |
| `tests/*.rigtest.hcl` | tests | |
| `stack.hcl` | a stack: the `rig` block and its modules | |
| `secrets.hcl` | the `secret` blocks of a stack | |
| `rig.lock.hcl` | the lock of a stack | |

- **The extension decides the kind**, as it does for packs; the `.tpl` goes
  because the files are HCL now. A job is `<name>.norca.hcl`: the name in
  use is Norca (decided by the maintainer), as in the job files of the
  registry's operators.
- **One resource per resource file.** The label of its block is the name the
  module refers to it by; the file name before the extension is the same.
- **The ID of a job is its `id` attribute**, the label when it has none (as
  Norca's decoder does). A module that can be installed twice in a namespace
  sets it from the release (`id = local.job`), or two releases would write
  one job (found by the conversion of section 14).
- **The content of a resource file is not thought again either.** Each kind
  keeps the format Norca reads for it today (`policy "<name>" { rules,
  job_acl }`, `kind "<name>" { ... }`, the job specification), with
  expressions in the values and the few attributes Rig adds (`enabled`,
  `for_each`, `hook`). What exactly each of today's formats looks like is
  taken from Norca when the kind is implemented, not redefined here.
- A block in a file of the wrong name is an error (a `job` in `locals.hcl`,
  a `variable` in `outputs.hcl`); an unknown file in a module is an error
  outside `files/`, `examples/`, `tests/` and the documentation files.

## 5. Resources of a module

One file per resource (section 4.5), of the kinds the releases know, applied
in the server's fixed order: namespace, quota, ACL policy, Variable, object
kind, admission policy, external service, object, job.

- The kind comes from the file extension, so the `variable` block of a
  `.var.hcl` file (a Norca Variable) and the `variable` block of
  `variables.hcl` (an input of the module) do not meet.
- Every resource takes `enabled` (bool), `for_each` or `count`. `for_each`
  takes a map or an object (`each.key`, `each.value`) or a collection of
  strings (both are the string); the copies are named `x["key"]` and `x[0]`.
  The meta-arguments are taken off the block before the job decoder sees it.
- An attribute that evaluates to null is left out, as if it were not
  written: `args = cond ? [...] : null` replaces `[[ if ]]` around one line.
- A resource that is not a job is, until its kind is implemented, the value
  of its block as JSON: attributes as they are, blocks by type, labelled
  blocks as an object by label, a block type written more than once (or a
  `dynamic` block) as a list.
- Hooks stay jobs with a `hook { on = "post-install", weight, timeout,
  delete }` block instead of meta keys. `on` is a phase or a list of them
  (`pre-install`, `post-install`, `pre-upgrade`, `post-upgrade`,
  `pre-delete`, `post-delete`, `test`), `timeout` a duration, `delete` one
  of `on-success`, `always`, `never`. Rig takes the block off the job, checks
  it and hands it to the host, which writes it the way its release API reads
  hooks: for Norca today the meta keys `pack.hook`, `pack.hook_weight`,
  `pack.hook_timeout`, `pack.hook_delete`.
- `check "<name>" { condition, error_message }` inside a module: a condition
  over evaluated values that stops the plan (for what a `rule` over variables
  can't say).
- A kind Rig does not know is refused. New kinds come by registration
  (section 11), so that a kind added to the server (volumes, network policy)
  needs no new language.

## 6. Variables, values, stacks

### 6.1 Sources of values

Order, last wins: defaults of the module, the stack's values, variable files
(`-f`), environment (`RIG_VAR_<name>`), `--var`. A value for an undeclared
variable is an error; a variable without a value and default is an error. All
values are checked (`validation`, `rule`) before evaluation, and a stack
checks every module before it changes anything.

### 6.2 Validation added

`unique_by = "<attribute>"` for lists of objects, `one_of = [...]`,
`item_of = [...]` for the items of a list,
`key_pattern` for the keys of a map, and `attribute "<name>" { ... }` for the
attributes of an object type (also of the objects of a list). With these, every
`fail` of today's packs can be written as a check (to be confirmed while
converting, section 14).

As built in phase 1: the constraints of a `validation` block are `pattern`,
`min_length`, `max_length` (a string), `min`, `max`, `integer` (a number),
`min_items`, `max_items` (a list, a set or a map), `unique`, `unique_by`,
`item_of` (a list or a set), `key_pattern` (a map), `one_of` (any value),
and the blocks `attribute "<name>" { }` and `item { }`, which hold the same
constraints for an attribute or for every item. `item { }` replaces the
`item_pattern`, `item_min_length` and `item_options` of the packs. A
constraint that does not fit the type of its variable is an error when the
module loads. The hints of the form (`ui`) are `tab`, `group`, `order`,
`label`, `widget`, `unit`, `presets`, `option_labels`, `show_if`, `help` and
`placeholder`; the options of a choice are its `one_of` (section 14.1).

### 6.3 Secrets

- A module never takes a secret as a plain variable. `secret { path, item }`
  says where the built-in secret store holds it; the job reads it at run time.
- `sensitive = true` marks a value that may not appear in a plan output, a
  log or a revision. A sensitive variable is not a plain one: a module may
  take a secret that way when the install form asks for it (the password of
  the redis pack). The mark goes with the value, so an item of a Variable
  resource that takes it becomes a secret item of the release
  (`SecretKeys`, `Secrets` of the release API), never stored in a revision.
  Anywhere else a sensitive value is an error: in a job (the decoder would
  write it into the job, which is stored), in another kind, in a
  meta-argument, and in an output that does not say `sensitive = true`.
  `rig module render` names the secret items and never writes their values.
- A stack may *create* secrets once, as the deployer's `secrets.bootstrap`
  does (create-once unless `overwrite`).

### 6.4 A stack

```hcl
# stack.hcl
rig {
  required_version = ">= 0.1.0"
  region           = "eu"

  registry "norca" {
    source = "github.com/bacanoo-io/norca-registry"
  }

  trust {
    require_signature = true
    public_keys       = [file("keys/norca-registry.pub")]
  }
}

module "rustfs" {
  source    = "norca/rustfs"
  version   = "~> 0.2"
  namespace = "platform"
  values    = { count = 1 }
}

module "forgejo" {
  source    = "norca/forgejo"
  version   = "0.2.0"
  namespace = "platform"

  values = {
    s3_endpoint = module.rustfs.endpoint     # an output of another release
    admin_token = "!generate:forgejo-admin"  # the deployer's value forms
  }

  # optional settings of the release
  atomic      = true
  timeout     = "10m"
  max_history = 10
}

module "worker" {
  source = "./modules/worker"                # a local module
}
```

- The label is the release name (`^[a-z0-9][a-z0-9-]{0,62}$`, the rule of the
  deployer).
- **A stack has modules of its own.** `source = "./modules/<name>"` is a
  module that lives in the directory of the stack. It needs no registry, tag,
  version (its `module` block may leave `version` out) or signature and is not in the lock file: it is part of the stack's
  repository and versioned with it. Registry modules and own modules are
  wired the same way (`examples/stack/platform`).
- A stack may declare `variable`s (filled from `-f` value files per
  environment), `locals`, `output`s, and `secret "<path>" { items }` blocks
  for the secrets it creates once.
- `enabled` on a module may depend on an output of another module.
- `values` is checked against the module's variables at `plan`.
- `enabled = false` keeps an entry without installing it; what happens to a
  release whose entry is removed is `prune` (section 9.3).

### 6.5 Dependencies

- **Between releases:** a reference `module.<a>.<output>` or
  `depends_on = [module.a]` orders the applies; Rig waits until the release it
  depends on is ready (its deployments healthy) before it applies the next.
  Cycles are an error at `plan`.
- **Declared by a module:** `requires { module "rustfs" { version = ">= 0.2" } }`
  in `metadata.hcl`; with `source = "<address>"` the label is a name of the
  module's own for the requirement and the address says which module. A stack that installs the module without one that satisfies
  it gets an error naming the missing module (today this is a sentence in a
  README).
- A `requires { module }` entry may carry `when = "<variable path>"`, so a
  module needs another one only with a setting on (dumps need an S3 store).
- **Inside a module:** child modules (`module "x" { source = "../lib/placement" }`)
  are evaluated into the same release. A module with `library = true` has no
  resources and can only be called this way. They replace the helpers copied between
  packs (`<pack>.location` six times, `image@digest` four times).
  As built in phase 1: a local source (`./x`, `../x`) is relative to the
  module that calls it and is read from the same root as the module (the
  repository the CLI finds it in; the fetched content on a server), never
  outside it. The other attributes of the block are values for the child's
  variables; locals and module blocks read each other in the order of their
  references (a cycle is an error). Inside the child `rig.module.*` is the
  child's; its resources join the release as `<block>.<name>`; a sensitive
  value goes only into a sensitive variable of the child, and a sensitive
  output of the child stays sensitive. Calls nest at most `Limits.Depth`
  deep. Open for phase 4: the digest of a module whose local child lies
  outside its directory must cover the child.
- Outputs are known at evaluation. An output can't read the running cluster
  (an address chosen at run time is found through the service catalog by the
  workload, as today).

## 7. Registries, addresses, versions

### 7.1 Sources

| Source | Example | Use |
| --- | --- | --- |
| global registry | `norca/nats` | the normal way: namespace/name in the global registry (7.3) |
| git | `github.com/acme/rig-modules//nats` | a private or unlisted repository |
| OCI | `oci://registry.internal/rig/nats` | internal registries, air-gapped |
| directory | `./modules/worker`, `dir:///opt/rig/modules` | development, shipped with a product |
| bundle | `bundle://./platform.rigbundle` | air-gapped transfer |
| built-in | `builtin/redis`, `builtin/audit` | modules embedded in the binary (the audit base: section 12.8) |

Registry backends are an interface (section 11).

### 7.2 Versions

- Every module has its own semver in `metadata.hcl`.
- In a git repository with several modules a version is the tag
  `<module>/v<version>` (the Go convention), so one module can be released
  without the others; a repository with one module uses `v<version>`.
- A tag is never moved. Rig stores the commit and the tree digest in the lock
  file and refuses a tag that points elsewhere.
- Version constraints as in the maintainer's sketch (`~> 1.2`, `>= 1.0, < 2`).
- **Stacks are versioned the same way** (section 7.5): a stack of a registry
  has its own semver and its own tag, `<stack>/v<version>`. Names are unique
  in a registry across modules and stacks.
- **There are no versions of a whole registry** (decided, question 3): the
  registry tags of today (`v0.2.0` of everything) are not carried over. A
  set of module versions tested together is a published stack with its lock
  (section 7.5).

### 7.3 The global registry (the later official repository)

One large public registry on the model of `opentofu/registry`, as the
maintainer set out (2026-11-15): **the modules that users define move into
it, and users add stacks that build on it.** Its repository is
`github.com/bacanoo-io/norca-global-registry` (created, empty; checked out at
`E:\orchestrate\norca-global-registry`). It is the default registry of
Rig: `source = "acme/nats"` means the module `nats` of the namespace `acme`
in the global registry.

Like the OpenTofu registry it holds **entries, not the sources**: a git
repository of small generated files, served as static files; no server to
run. The code of a module stays in the repository of its author.

```
modules/<first letter>/<namespace>/<name>.json     # versions of one module
stacks/<first letter>/<namespace>/<name>.json      # versions of one stack
namespaces/<namespace>.json                        # the owner and the source repositories
keys/<namespace>/*.pub                             # signing keys of the namespace
```

- **A namespace** belongs to a user or an organization (the owner of the
  source repositories). The official modules of Norca live in the namespace
  `norca`.
- **An entry** lists, per version: the source (repository, tag, commit), the
  tree digest, the signatures (key ID + signature), `requires`, the icon
  digest; for a stack also the modules its lock pins.
- **Getting in:** after the review of a request, a namespace, its keys and
  its source repositories are added; from then on a workflow generates the
  entries from the tags of those repositories. CI checks every entry: the tag exists, the digest
  matches, the signature verifies with a key of the namespace, the module or
  stack validates.
- **Stacks of the global registry build on the global registry.** Every
  registry module a listed stack uses must itself be listed there (any
  namespace), at the version its lock pins; its own modules in `modules/`
  travel with it. A stack that reaches for a module outside the global
  registry is not accepted. So everything a listed stack installs can be
  found, inspected and verified in one place.
- **Append-only, with versions** (the maintainer's rule, 2026-11-15):
  - a module or a stack **enters only after the checks** passed; nothing is
    listed unchecked;
  - a listed version is **never updated and never deleted**: its source,
    digest and signatures stay as they were accepted;
  - the only change is **a new version** of the module or stack, which goes
    through the same checks.
  - **The one exception is removal in four cases** (decided, question 15):
    malicious code or a supply chain compromise, exposed secrets, a
    compromised signing key, a legal requirement. It needs a request with
    evidence and a recorded decision; the version then goes onto the blocked
    list with its reason, so it is not listed again and a lock that names it
    fails with that reason. Never for a CI mistake, a small bug or a change
    of mind.

  So a lock file that names a version of the global registry stays valid for
  good, and what was reviewed is what is installed. A faulty version is
  answered by a newer one, not by changing or removing the old. CI of the
  registry repository refuses a pull request that changes or removes an
  existing version entry.
- **One list of advisories and blocked versions** next to the entries
  (decided, question 15): namespace, name, version, kind (warning or
  blocked), reason. A warning is shown by `rig stack sync` and `plan` and
  changes nothing else; the entries themselves are not touched.
- **Getting listed** is asked for through an issue form and reviewed; a
  namespace, its keys and its repositories are added by the maintainers of
  the registry, not by direct pull requests (decided, question 15).

**What the OpenTofu registry does** (read 2026-11-15: its README, POLICY.md,
BLACKLIST_README.md and repository), as the reference; the maintainer accepted the right-hand column
(2026-11-15):

| Point | OpenTofu registry | Norca global registry |
| --- | --- | --- |
| What it holds | metadata only: one JSON per module with, per version, the version, the **commit** and when it was found; the source stays in the author's GitHub repository, the registry answers a download with a location | the same, plus the tree digest and the signature per version, so the content is verified even if a tag moves (decided, question 14) |
| How versions arrive | a job reads the tags of the listed repositories every 15 minutes | the same, after the checks of this section |
| Getting listed | an issue form, reviewed and approved or denied by the team; no direct pull requests | the same |
| Changing a version | never: "treats published versions as immutable"; fixes are new versions; not removed for CI mistakes, small bugs or a change of mind | the maintainer's rule above |
| Removing a version | exceptional only: malicious code or a supply chain compromise, exposed secrets, a compromised signing key, a legal requirement; a request with evidence, decided by a vote of the steering committee, carried out with two approvals | the same four cases, with a recorded decision (decided, question 15) |
| Keeping a bad version out | `versions_blacklist.json`: namespace, name, version, reason; the job skips it | one list of advisories and blocked versions with reasons, next to the entries (decided, question 15) |
| Warnings | a `warnings` list in the entry of a provider ("This provider is deprecated.") | warnings in the same list, shown by `rig stack sync` and `plan` |

Rig needs only "namespace/name → versions → source + digest + signature", so
the global registry is not the only way: every source of 7.1 works without
it (a private git repository, OCI, a directory), and an organization can run
a registry of the same shape for itself.

### 7.4 Layout of a module and of a registry repository

```
nats/
  metadata.hcl        the module (4.4)
  variables.hcl       variables and rules
  locals.hcl          locals and checks
  modules.hcl         child modules
  outputs.hcl         outputs
  nats.norca.hcl      one file per resource, named by kind (4.5)
  files/              files for file() and templatefile()
  examples/*.vars.hcl value files; rendered and validated in CI
  tests/*.rigtest.hcl assertions over the evaluated module, without a cluster
  icon.png            64x64 PNG, optional
  README.md
```

- **Icon:** `icon.png`, exactly 64x64 pixels, PNG, at most 32 KiB; checked by
  `rig module validate`; part of the tree digest; shown in the global registry and in
  the UI. Without it the default icon of Rig is used. PNG only (decided,
  question 8): an SVG can carry content that runs and would have to be
  cleaned before it is shown.
- Tests: `test "<name>" { release, values, assert { condition,
  error_message }, expect_error }`; assertions read `resource.<kind>.<name>`
  and `output.<name>`. As built (phase 1): `namespace` and `region` may be
  given too (default `default`, `global`; the release defaults to the
  module's name); a test expects an error or asserts, not both. The kind
  is the block type of the file (`resource.job`, `resource.policy`,
  `resource.variable`; `acl-policy` is no name). A resource reads as its
  block is written, not as the host encodes it, so that a test does not
  depend on the host: labelled blocks by label, a block written once as an
  object, more than once or `dynamic` as a list (an empty list when it
  makes none), null attributes kept, the `hook` block decoded, sensitive
  values marked (`issensitive()`), runtime values as their functions give
  them (`== runtime("node.datacenter")`). `count` makes a list,
  `for_each` an object by key; every kind is there, empty without
  resources. Lists, sets and maps read as tuples and objects, the types of
  the literals they are compared with. The module is evaluated with the
  host's job decoder, so its errors fail a test too. `rig module test`
  prints a line per test and exits 1 on a failure; `rig module validate`
  parses the tests.
- A registry repository: `modules/<name>/` and `stacks/<name>/`, `examples/`
  inside each of them, `LICENSES/`, `REUSE.toml`, `.gitattributes` with LF
  (the digest is over file contents), CI: `rig fmt -check`, `rig module
  validate`, `rig stack validate`, the examples, the tests, REUSE and DCO.

### 7.5 Stacks in a registry

A registry holds stacks as well as modules: a tested composition (a database
with its backup store, a messaging platform, a whole product) that others
install as one thing, with their own values.

```
stacks/data-platform/
  metadata.hcl        the `stack` block: name, version, description, license, requires, icon
  stack.hcl           the `rig` block, the modules and how they are wired
  variables.hcl       the inputs of the stack, with validation and form hints
  outputs.hcl         what the stack tells its user
  modules/            the stack's own modules (part of its digest)
  rig.lock.hcl        the versions and digests of the registry modules it uses
  examples/*.vars.hcl value files; planned in CI
  icon.png, README.md
```

- **Metadata** is a `stack { ... }` block in `metadata.hcl`, with the same
  fields as a module (name, version, description, url, license, category,
  keywords, icon, `requires { norca, features }`).
- **Its modules** are registry modules (by address and version constraint,
  never by a path into the repository, also for modules of the same
  registry) and modules of its own in `modules/`. The registry modules may
  come from **another registry**: the stack names that registry and carries
  the public keys its modules are signed with, so the user of the stack
  trusts one key, the stack's (`examples/registry/stacks/observability`). The own modules travel with the
  stack and are covered by its digest; they are not published separately.
- **The lock file is published with the stack.** A version of a stack names
  exact module versions and digests, so everyone who installs
  `data-platform 1.2.0` gets the same modules. `rig stack publish` refuses a
  stack whose lock is missing or does not match.
- **Identity and signature** are those of a module: the tree digest of the
  stack directory (own modules and lock included), signed the same way,
  trusted by the same policy. The modules it fetches are verified by their
  own digests from the lock.
- **Using it:**

  ```shell
  rig stack plan  example/data-platform --version "~> 1.2" -f my.vars.hcl
  rig stack apply example/data-platform --version "~> 1.2" -f my.vars.hcl
  ```

  The releases record the stack's name and version as their owner, so
  `status`, drift and `--prune` work as for a local stack. The local
  directory holds only the values and the `.rig/` data.
- **Building on it:** a local stack may take a registry stack as a part,
  `stack "data" { source = "example/data-platform", version = "~> 1.2",
  values = { ... } }`, and wire its own modules to the outputs
  (`stack.data.<output>`). The releases of the part are named
  `<part>-<release>` unless the part sets names. **One level only**
  (decided, question 13): a local stack over registry stacks; a registry
  stack has no `stack` parts.
- **Global registry:** stacks are listed next to modules (section 7.3) with their
  versions, digest, signatures, icon and the modules each version locks.

## 8. Supply chain

### 8.1 What is signed

The **tree digest** of the module version, with the module name and version,
in one small signed statement. The same statement is valid whether the module
came from git, OCI, a directory or a bundle.

The statement is a cosign *simple signing* payload whose
`critical.image.docker-manifest-digest` is the tree digest
(`sha256:<hex>`) and whose `optional` part names the module and the version
(`rig.module`, `rig.version`). This is the payload the admission of Norca
verifies for `trusted_keys` today, so a release of a signed module passes it
without a change in norca (the spike of phase 0, section 12.2). Signatures are stored where the
version is published: in the entry of the global registry, as a `.sig` file next to a
directory or bundle, as a cosign signature in OCI, as a release asset or a
signed tag note in git.

- Keys: the cosign-format ECDSA P-256 keys of today (`rig key generate`,
  `rig sign`, `rig verify`).
- Keyless signing (Sigstore: OIDC identity of a workflow, transparency log)
  is in the maintainer's sketch and is not in the pack manager today. It needs
  network access to Sigstore services at verify time or a bundled proof,
  which a closed network does not have. **Keys first** (decided, question 5);
  keyless comes later as a second signer type behind the same interface.

### 8.2 Trust

- `trust { require_signature, public_keys }` per registry and as a default in
  the stack and in the user configuration; checked at download, at every use
  of the cache, at `plan` and at `apply`.
- The release records the module's tree digest and the key IDs that signed
  it, so the server's admission policy for releases (`trusted_keys`) can check
  the same thing it checks today.
- While the client evaluates (section 9.1) the server can't prove that the
  applied resources came from that digest: the gap of today's client-rendered
  packs. It closes when the server embeds Rig and evaluates itself
  (`ServerRendered`).

### 8.3 Lock file

`rig.lock.hcl` next to a stack: per module the resolved version, the source,
the commit or manifest, the tree digest, the key IDs. `--lock=verify`
(default in CI), `update`, `require`. A module with child modules from other
sources carries its own lock.

### 8.4 Access to private registries

As in the sketch: a token from the environment (`RIG_GITHUB_TOKEN`,
`GITHUB_TOKEN`), the `gh` session, SSH agent, OCI credentials (docker
config), `rig login` with the key store of the system. No secret in a stack
file.

## 9. Engine and apply

### 9.1 Who evaluates

Rig runs in two ways, and both are kept until the end of the work, when the
maintainer decides which stay (question 12):

- **Standalone (external application):** the `rig` program evaluates on the
  machine it runs on and sends the resources to the server's release API as
  it exists (apply of rendered resources, plan, history, rollback, drift,
  uninstall). Releases made this way are ordinary releases: visible in the
  UI, guarded by admission, with revisions. This is the only way while norca
  is not changed, and it needs local data of its own (section 12.6).
- **Embedded:** the server takes the library in, evaluates a module it
  fetched and verified itself, and serves the install form. A client (the
  deployer, the UI, a thin `rig`) then sends module, version and values, as
  the deployer does with packs today, and keeps nothing locally.

### 9.2 Steps of `plan` / `apply` (the deployer's principles)

1. **Load and validate everything first:** the stack, the lock, every module
   (schemas, values, rules, checks, `requires`, the dependency graph). Any
   error stops before a change.
2. **Resolve and verify:** versions, downloads into the cache, digests,
   signatures.
3. **Evaluate** each module to resources.
4. **Compare** with the cluster: per release, the digest of (module digest +
   values + settings) against what the release's current revision records;
   then the server's plan for the resources that differ.
5. **Show** (`plan`): per release `unchanged`, `install`, `upgrade` with the
   per-resource create/update/delete of the server's plan, `remove`. Exit
   codes for CI: 0 nothing to do, 2 changes, 1 error.
6. **Apply** in dependency order, each release through its own lock on the
   server, waiting until it is ready. The first failure stops the rest; a
   second `apply` continues where it stopped, because step 4 skips what is
   done.

### 9.3 State

There is no state file. What is installed is the releases of the cluster:

- "unchanged (skipped)" comes from the digest recorded in the release, so it
  is right from any machine and after a restore;
- drift is the server's drift check, shown by `rig stack status` / `plan`;
- **prune:** a release that carries the stack's name as its owner and is no
  longer in the stack is shown as `remove`; it is uninstalled only with
  `apply --prune` (the deployer never uninstalls on a removed entry; silent
  removal would be a surprise).

### 9.4 Errors

HCL diagnostics with file, line and the expression, for every stage; the
server's errors are attached to the release and resource they belong to.

### 9.5 The log: a tree of operations

Decided by the maintainer (2026-11-17): the log shows which step belongs to
which operation, and which subtask to which task.

- **Every operation is a span** (package `oplog`). Its start line and its
  end line (`op`, `duration`, `outcome`) and every line logged inside it
  carry `trace_id` (of the root operation), `span_id`, `parent_span_id`
  and `depth`. The keys are those of the key convention of Norca's
  OpenTelemetry plan (`norca/design/opentelemetry-plan.md`, section 2).
- **The tree of a stack** (the names are a proposal, fixed when the
  operations are built):

  ```
  stack.apply                 stack, region            info
    stack.load                                         debug
    module.resolve            source, version          debug
      module.fetch, module.verify                      debug
    release.evaluate          release, module, version debug
    release.plan              release                  info
    release.apply             release, revision        info
      release.wait            release                  debug
    release.prune             release                  info
  ```

  `stack.plan`, `module.render` and `module.test` (a span per test) are
  roots the same way. Lines per resource are debug.
- **An error is not logged on the way up.** The end line of a failed span
  says `outcome=error` without the error; the error is returned as a
  diagnostic and reported once by the host (the rule of a single handling).
- **Every line is logged with a context** (the `Context` variants of
  slog); the linter enforces it (`sloglint`, `context: all`). A line
  without a context belongs to no span.
- **The rig program** writes the tree for a person (`-log-format text`:
  indented by depth, the operation first, without the IDs) or flat JSON
  lines with the IDs, from which the tree is rebuilt (`-log-format json`).
- **Embedded** the server's handler gets the same IDs. When the
  OpenTelemetry plan of Norca is built, a host with a tracer gets
  OpenTelemetry spans with the same IDs (`oplog` uses their sizes); until
  then the core has no dependency on OpenTelemetry.

## 10. CLI

```
rig init                         a stack or a module from a skeleton
rig fmt [-check]                 format (all files are HCL, nothing is skipped)
rig module validate [dir]        schemas, values of the examples, icon, tests
rig module show <source>         variables, outputs, requires, versions
rig module test [dir]            tests/*.rigtest.hcl: values in, assertions over resources
rig module render <source>       the evaluated resources and outputs as JSON, for review
                                 (-f values, -var name=value, -release, -namespace, -region)
rig module publish               tag, sign, (entry of the global registry)
rig registry add|list|remove     registries of the user configuration
rig login <host>
rig key generate / sign / verify
rig bundle create|inspect|import

rig stack sync                   resolve versions, fill the cache, write the lock
rig stack plan                   what would change
rig stack apply [--prune]        make it so
rig stack status                 releases of the stack, readiness, drift
rig stack destroy                uninstall the releases of the stack
rig stack validate [dir]         schemas, values of the examples, lock, icon
rig stack show <source>          variables, outputs, the modules a version locks
rig stack publish                tag, sign, (entry of the global registry); needs a matching lock

Every `rig stack` command takes a directory (default: the current one) or a
registry address with `--version`.

rig release list|status|history|rollback|uninstall    one release, without a stack
rig install <source> --name x -f values.hcl            a stack of one, for quick use
```

`rig install` covers what `norca pack run` does today; everything else of a
single release goes to the server's release API unchanged.

## 11. Extension points

Everything is behind an interface with a registry in the library; the first
version registers the built-in ones at compile time.

| Point | Built in | Others add |
| --- | --- | --- |
| Functions | the HCL standard set, encoders (`jsonencode`, `yamlencode`, `tomlencode`, `natsconf`), `image_ref`, `file` | Go functions (`cty` function) |
| Resource kinds | the nine of section 5 | a schema + a mapping to a release resource |
| Registry backends | git, OCI, directory, bundle, built-in, the global registry | any fetcher (S3, an internal catalog) |
| Signers / verifiers | cosign-format keys | keyless, KMS, PGP |
| Value forms | `!generate`, `!nkey-*`, `!enc:`, `@file` | new prefixes |
| Form widgets | those of the UI today | by name; unknown ones fall back to a text field |
| Output of `plan` | text, JSON | formats |

**Extensions are registered at compile time only** (decided and confirmed,
question 7): there is no plugin protocol for extensions outside the binary.

- Who writes modules and stacks extends freely with modules, child modules
  and stacks; that needs no Go and no build.
- Who adds to the engine (a function, a resource kind, a registry backend, a
  signer, a value form) writes Go against the interface and builds `rig`, or
  the Norca server with it. No program of a third party is fetched and run
  by Rig, also not on the leader.
- The interfaces are drawn so that a plugin protocol can be added later
  without changing modules.

## 12. The library interface: what Norca takes in

Rig is a Go module with a public interface that a host program embeds. The
`rig` program is the first host (standalone); the Norca server is the next
(embedded). This interface is designed first and the CLI is written against it, so
that the server gets nothing less than the CLI has.

### 12.1 Direction of dependencies and the layout of the code

Decided with the maintainer (2026-11-15), following the Go layout rules we
use (the golang skills): **hexagonal architecture** and **manual constructor
injection**.

- **The core imports nothing of Norca.** Not the server, not its structs, not
  its API client. Norca imports Rig, never the other way; there is no cycle
  and the server does not pull a second copy of itself.
- **Ports and adapters.** What the core needs from outside it asks through
  interfaces (ports) in `host/` and `source/`. The adapters are separate:
  the one over the Norca HTTP API and the local store for the standalone
  program, the server's own inside norca.
- **No DI library.** A host passes its adapters and extensions to
  `rig.New` as functional options (`rig.WithCluster(c)`,
  `rig.WithReleases(r)`, ...; decided by the maintainer, 2026-11-17); an
  option with an invalid argument makes `New` fail. Everything else is
  built by constructors. A library that others embed brings as few
  dependencies as it can.
- **Two Go modules in one repository**, so that the core's `go.mod` names no
  Norca module: the core at the root, and the standalone program (CLI and
  its adapters, which need the Norca API client and job decoder) in `cli/`,
  joined by `go.work` for development.

```
norca-rig/
  go.mod                  module github.com/bacanoo-io/norca-rig  (the core; no Norca dependency)
  go.work                 the root and ./cli, for development
  rig.go                  package rig: Engine, New(...Option), Load, Evaluate, PlanStack, ApplyStack
  example_test.go         runnable examples of the public API

  module/                 a module: metadata, variables, form schema, digest
  stack/                  a stack, its graph, a plan
  resource/               the evaluated resources: plain, serializable data
  host/                   ports a host implements: Cluster, JobDecoder, Releases, Secrets, Store
  source/                 port Fetcher and the built-in fetchers: git, OCI, directory, bundle, global registry
  trust/                  digest, signature, policy, lock
  extension/              registration of functions, resource kinds, value forms, signers

  internal/               private to the core
    lang/                   loading HCL, schemas, file-name rules, diagnostics
    eval/                   evaluation context, functions, dynamic blocks, runtime references
    planner/                comparing with the releases, the order of applies
    builtin/                modules embedded with go:embed

  cli/                    module github.com/bacanoo-io/norca-rig/cli  (the standalone program)
    go.mod                  requires the core and the Norca API client
    cmd/rig/main.go         only main: parse the arguments, wire, run
    norcaapi/               adapter: the host ports over the Norca HTTP API and its job decoder (public: the deployer may link it)
    internal/jobspec2/      until phase 7: a copy of norca's jobspec2 with Rig's DecodeJob (question 4)
    localstore/             adapter: Store on the user's and the stack's directories
    internal/command/       the commands of the CLI

  doc/                    the documentation site (section 13.2)
  design/                 this design
  examples/               modules, a registry and a stack in the language
  Makefile  .golangci.yml  .gitignore  LICENSE  README.md
```

- Public packages are at the root of each module, named in lower case and
  the singular after what they hold; `internal/` holds what no importer may
  use; `cmd/rig` has `main.go` only.
- Tests sit next to the code (`_test.go`), fixtures in `testdata/`; the
  modules of `examples/` are test input as well.
- The CLI is configured by flags and environment variables and logs to
  standard error; the core reads neither (section 12.4).

### 12.2 What the host provides

| Interface | The core asks | Standalone | Embedded in the server |
| --- | --- | --- | --- |
| `Cluster` | version, features, region, namespaces (for `requires` and `rig.*`) | HTTP API | its own state |
| `JobDecoder` | turn an evaluated `job` block into a job the cluster accepts | the jobspec decoder, linked into `cli/norcaapi` | its own decoder |
| `Releases` | current revision and its digest, plan, apply, history, rollback, uninstall, drift; composed of small interfaces (`ReleaseReader`, `ReleasePlanner`, `ReleaseWriter`; decided by the maintainer, 2026-11-17) | release API | release endpoints, in process |
| `Secrets` | create-once secrets of a stack | secret API | secret store |
| `Fetcher` (optional) | bytes of a module version, when the host has its own cache | the built-in sources | the leader's registry cache |
| `Store` | the cache of modules, the configuration, values that must outlive a run (section 12.6) | a directory of the user and of the stack | the leader's data directory and objects in Raft |
| `Clock` | time | the system clock | the system clock |
| logging | a `*slog.Logger` of the standard library, not a port of Rig (decided by the maintainer, 2026-11-17; the controller SDK of norca takes one the same way); `slog.Default()` when the host gives none; the engine wraps its handler so that the log is a tree of operations (section 9.5) | a tree (text) or JSON on standard error | a handler that writes into the server's hclog |

The job decoder is the host's because the job specification belongs to
Norca: the core evaluates expressions and hands over a block with all
install-time values resolved; the same decoder then serves the CLI and the
server, and the core never follows the job schema by hand (this answers most
of question 4).

What the spike of phase 0 showed (2026-11-17; its test is now
`cli/internal/jobspec2/rig_decode_test.go`):

- The core does not hand over a resolved block but the block as parsed and
  an evaluation context (`host.JobSpec`): `var.*`, `local.*`, `module.*`,
  `rig.*`, the functions of Rig, and an `UndefinedVariable` callback that
  gives a runtime reference back as its text. This is the mechanism of
  Norca's own decoder (`jobspec2`, `types.config.go`), so `dynamic` blocks
  and runtime references behave as in a job file today.
- `UndefinedVariable` is a field of the hcl commit Norca pins
  (`v2.20.2-0.20240517235513-55d9c02d147d`, the Nomad branch); upstream hcl
  (v2.24.0) has no such field. **The core must stay on the hcl version of
  Norca**: if it required a newer upstream hcl, the server that embeds it
  would get that version and its decoder would not compile.
- Norca's decoder passes every unknown root through, so a misspelled
  install-time reference (`rig.relase`, `vars.x`) would end up in the job
  as text. The callback of Rig must refuse the roots of Rig's namespaces
  and names close to them, with a hint (section 4.2).

What the spike of the release API showed (2026-11-17, against a dev agent;
`cli/norcaapi/release_integration_test.go`, `make integration`). The release
API of today takes what Rig evaluates, without a pack catalog and without a
change in norca:

| Rig | In the request (`ReleaseApplyRequest`) | Kept in |
| --- | --- | --- |
| the module: name, version, source, tag, tree digest, signatures, `requires` | `Pack` (`Name`, `Version`, `Source`, `Ref`, `Digest`, `Signatures`, `Requires`) | the revision and the release; `Requires` is checked by the server |
| the digest of a release (module digest, values, settings; section 9.2) | `ValuesHash` | the revision (opaque to the server) |
| the values without the sensitive ones | `Values` | the revision |
| the stack that owns the release | `Description` (`rig stack <name>`) | the revision and the release; `List` returns it, so prune needs no revision reads |
| the evaluated resources | `Resources`, typed by kind (`job`, `variable` ...: the names of `resource.Kind`) | the revision (`ContentDigest` is the server's digest of them) |

- The server's plan of an unchanged request says `unchanged` per resource,
  but an apply of it makes a new revision all the same: "unchanged
  (skipped)" is Rig's, by comparing `Pack.Digest` and `ValuesHash` with the
  current revision before it applies (section 9.2, step 4).
- With an admission policy with `trusted_keys` on the namespace, an
  unsigned module is refused ("not signed"), and a module signed as in
  section 8.1 is admitted with `TrustedSigner` set to the key ID (the
  SHA-256 of the public key).
- Gaps for phase 7: the owner has no field of its own (the deployer writes
  the provenance of GitOps into `Description` as well; once the deployer
  runs stacks through Rig the marker of Rig is the one written);
  `ValuesHash` carries more than the values; the server still can't check
  that the digest belongs to the resources (section 8.2).

### 12.3 What the core offers

```go
eng, err := rig.New(rig.WithCluster(c), rig.WithJobDecoder(d),
    rig.WithReleases(r), rig.WithLogger(logger), rig.WithLimits(limits))

mod, diags := eng.Load(ctx, rig.Ref{Source: "norca/nats", Version: "~> 1.0"})
form := mod.FormSchema()                       // the install form of the UI
res, diags := eng.Evaluate(ctx, mod, rig.Install{
    Release: "nats", Namespace: "platform", Region: "eu", Values: values})
// res.Resources, res.Outputs, res.Digest, res.Signers

plan, diags := eng.PlanStack(ctx, stack)       // what would change
diags = eng.ApplyStack(ctx, plan, rig.ApplyOptions{Prune: false})
```

- `Load` fetches, verifies and parses; `Evaluate` is a pure function of the
  module, the values and the install facts: same input, same resources and
  same digest, on a laptop and on the leader.
- Everything returned is plain data that serializes (JSON, msgpack), so the
  server can pass it through its RPC and store it in a revision.
- Errors are diagnostics with a source position, never a panic.

### 12.4 Rules the core keeps, because a server runs it

The server evaluates modules that other people wrote, on the leader, with the
leader's rights. So the core:

- has **no global state** and is safe for concurrent use; every call takes a
  `context` and stops when it is cancelled;
- reads **only what it is given**: modules as `fs.FS`, values as data; no
  environment variables, no working directory, no home directory inside the
  core (the CLI reads those and passes them in);
- lets a module reach **nothing outside itself**: `file()` stays inside the
  module, there is no function that runs a program, opens a connection or
  reads the host's files; a host may add functions, a module can't;
- has **limits** the host sets: size of a module and of a file, number of
  resources, depth and steps of evaluation, time;
- touches the network only through `Source` / `Fetcher` and the cluster only
  through the host;
- keeps a **stable, versioned API** (semver of the Go module); a host built
  against 1.x keeps working with a later 1.y.

### 12.5 How Norca uses it later (phase 7)

- The server implements the host interfaces, embeds the core, and evaluates
  a module it fetched and verified itself: the release is server-rendered and
  the signature covers what was applied (section 8.2).
- The install form of the UI comes from `FormSchema`.
- `norca rig ...` commands, if wanted, are the CLI package linked into the
  `norca` binary.
- The deployer needs no engine of its own then: it sends module, version and
  values to the server, the way it sends packs today (through the `norca`
  command on the first server). While the standalone way exists, the
  deployer may instead run a stack through it; which of the two it uses is
  part of the final decision (question 12).

### 12.6 Local data of the standalone program

Embedded, Rig keeps everything in the server. As an external application it
has to keep data on the machine it runs on. The rule from section 9.3 stays:
**local data is never the record of what is installed** (that is the
releases of the cluster). What is kept locally, and what its loss costs:

| Data | Where | If lost |
| --- | --- | --- |
| Module cache: fetched versions by digest, with the proof of their origin (source, commit, signatures) so that a cached module is verified again before every use | user cache directory (`RIG_CACHE_DIR`) | fetched again |
| Copies of global registry entries | user cache directory | fetched again |
| Configuration: registries, default trust policy and public keys, cluster profiles (address, CA, which credential) | user configuration directory (`RIG_CONFIG`) | written again by the user |
| Credentials: registry tokens, cluster tokens | the key store of the system; an encrypted file where there is none | `rig login` again |
| Signing keys (private) | where the user puts them, encrypted with a passphrase | **not recoverable**: a new key and a new trust entry |
| Lock file | next to the stack, in version control | `rig stack sync` writes it again (versions may move within the constraints) |
| Generated values of a stack (`!generate`, `!nkey-seed`) | `.rig/` next to the stack, encrypted with a master key as the deployer does | **a new value would be generated**: see below |
| A saved plan (`rig stack plan -out`) | `.rig/` next to the stack | planned again |
| Journal of the last apply (what was started, for a clear message after an interrupt) | `.rig/` next to the stack | none: the next `plan` reads the cluster |

- **Generated values are the one piece of local data with weight.** If the
  local copy were the only one, losing the directory would change a password
  on the next apply. So a generated value is also written once into the
  secret store of the cluster (create-once, as `secrets.bootstrap` of the
  deployer), and Rig reads it back from there when the local copy is
  missing. The local copy is then a convenience, not the source.
- Everything under `.rig/` that holds a secret is encrypted (AES-256-GCM,
  the deployer's `!enc:` form and master key sources), and `.rig/` is not for
  version control (a `.gitignore` is written with it).
- The cache is safe to share between stacks and users of one machine only
  as far as verification goes: content is addressed by digest and checked on
  use; credentials are never in it.
- Two runs against one stack at a time: a lock file in `.rig/` stops the
  second on the same machine; across machines the server's per-release lock
  is what protects a release.
- All of this is behind the `Store` interface (12.2), so the same engine
  uses a directory when standalone and the server's storage when embedded.

### 12.7 Option: evaluation on an observer, apply on the leader

An option for the embedded way, recorded on the maintainer's word
(2026-11-15); not planned for a phase yet.

**Today** (checked in norca): every server, an observer too, serves the HTTP
API and the web UI, validates a job statically, runs the capacity dry run
and serves stale reads and the event stream from its own state. Everything
of the pack registries is the leader's: all registry calls are forwarded to
it, the registry cache is in its data directory, it syncs, verifies
signatures, renders, and applies a release under a lock it holds in memory.
So the admin UI can run on an observer, but the registry work it starts
lands on the leader.

**With Rig** the split can change, because evaluation is a pure function
(section 12.3): the same module, values and install facts give the same
resources and the same digest on any server.

| Step | Where | Why |
| --- | --- | --- |
| Fetch a module or stack, keep the cache | the observer that got the request | network and disk work; the cache is per server (`Store`) |
| Verify digests and signatures | the observer | CPU work on content it holds |
| Evaluate, build the install form, plan | the observer | pure function |
| Checks that read the state of the cluster (quotas, node pools, admission, ownership) | the leader | an observer's state may lag |
| Apply: the release lock, the writes, hooks, waiting for deployments | the leader | Raft writes and the lock are the leader's |

What has to hold for it:

1. **Trust.** "Server-rendered" means today that the leader fetched and
   verified the content itself. The leader must not take an observer's
   resources on its word: the observer sends the module digest, the values
   and the resources, and the leader either evaluates again from its own
   verified copy when it has one, or checks the signed digest and that the
   sender is a server of the region (the server certificate). Which of the
   two is the rule is to decide when this is built.
2. **A cache on every server that serves it**, kept by that server; a
   server without the content fetches it or forwards the request.
3. **The same library version** on the observer and the leader, or the
   digests of the evaluated resources differ; a server refuses to evaluate
   for a leader that reports another version, and forwards instead.
4. **The install facts come from the leader** (the namespace, the region,
   the features): the observer asks for them with the request it prepares,
   so a lagging state can't change the result.

What it gives: the fetching, verifying and evaluating of many modules, the
install forms of the UI and the plans of large stacks leave the leader; only
the apply stays. What it does not change: every write still goes through the
leader, and an observer never serves a consistent read.

The interfaces of 12.2 already allow it (the `Fetcher`, `Store` and
`Releases` of a host need not be on the same server); nothing in the core
has to know.

### 12.8 The audit module: a base in Rig, on the internal bus when embedded

The maintainer's direction (2026-11-15); the details below are a proposal
and nothing of it is built.

- **The base of audit is a module of Rig**, embedded in the binary as the
  other built-in modules (`builtin/audit`, section 7.1). A stack imports it
  like any module, next to its other modules, and wires it with values and
  outputs. It is versioned with Rig and needs no signature or lock entry,
  being part of the binary.
- **Audit is declared, not wired** (the maintainer, 2026-11-15: the first
  sketch read like direct JetStream events, technical and not declarative).
  The author says **what** is audited, in the words of Norca, in a
  resource file of its own, **`<name>.audit.hcl`** (decided by the
  maintainer, 2026-11-15), one declaration per file like every other
  resource (section 4.5):

  ```hcl
  # shop.audit.hcl
  audit "shop" {
    namespaces = [rig.namespace]
    record     = ["job", "secret", "variable", "acl"]   # kinds of resources
    actions    = ["create", "update", "delete"]
    keep       = "90d"
    readers    = ["shop-auditor"]                       # ACL roles that may read the trail
  }
  ```

  It is a resource of a module: it takes `enabled`, `for_each` and
  expressions as the others do, it is part of the release, planned, applied
  and pruned with it, and any module may carry one for what it installs.
  The built-in module `builtin/audit` is the base these declarations stand
  on; a stack imports it once.

  Subjects, streams, consumers, JetStream domains, accounts, tokens and
  inboxes appear nowhere in what an author writes: the module derives them,
  and they may change without a stack changing. The same holds for reading:
  a reader asks for the records of a resource, a person or a time span, not
  for a consumer of a stream.
- **A record, not an event.** What the trail holds is a record with fixed
  fields: when, who (the accessor of the token or the workload identity),
  which action, which resource (kind, namespace, name), the outcome, the
  Raft index. Today's events don't carry all of it (who made a change is
  only in the admin events and in the audit of the secret store), so the
  full record needs work in norca and belongs to the integration; until
  then the module can only give what the events hold.
- **"Base"** means the common part every audit trail needs: what is
  recorded, how long it is kept, who may read it. What a product adds (a
  store of its own, reports, forwarding) is another module that takes the
  outputs of the base.
- **Where the changes come from depends on how Rig runs**, not on the module
  or its values:

| Way to run | How the module gets the changes |
| --- | --- |
| standalone program | from outside, through a leaf node: the listener, a `cdc` policy with `leaf = true`, a token, a NATS that attaches (the steps in `examples/stack/platform/AUDIT.md`) |
| embedded in the server (phase 7) | it **sits on the internal bus**: the server gives it the changes directly; no leaf listener, no token and no NATS in between |

- **The core still imports nothing of Norca** (12.1): the attachment to the
  bus is a port of the host. The server implements it over its system bus;
  the standalone host has no such port, and the module then takes the leaf
  node way. The module sees which way it has from the install facts
  (features).
- **The rules of change data capture hold in both ways**: one way only, the
  reader can neither overload the bus nor write to it, job events stay
  redacted, and what a reader gets is bounded by its rights.

To decide when this gets a phase: the names of the attributes and of the
fields of a record (the ones above only show the form); what exactly the
base module holds once the declarations are resources; the place of the
kind in the order of applies (section 5); what the standalone program does
with an `audit` resource while the server does not know the kind (a kind
the releases don't know is refused today); where the trail is stored and
for how
long (the event streams keep 72 hours by default), who may read it, and how
it meets the two postponed plans in norca (the history stream of the server
roles plan, the operator audit records of the OpenTelemetry plan).

## 13. Repositories and how we develop

### 13.1 How the deployer takes packs today, and stacks later

Checked in norca-deployer (`doc/packs.md`, `doc/gitops.md`,
`doc/how-apply-works.md`). It installs packs in two ways, both on `apply`:

| Today | From | How |
| --- | --- | --- |
| `packs.deploy` | **local files:** the entries and their values are written in the plan file; the pack comes from a registry of the cluster (git, OCI, a directory on the servers, built-in) | the server renders; a marker file per release decides "unchanged" |
| `packs.gitops` bindings | **a repository:** a pack directory in a Git repository (Forgejo, Gitea, GitHub, GitLab; branch or ref, subpath) | fetched into memory, optionally only from a signed commit or tag, uploaded to the cluster, deployed; also on a push through the webhook daemon, and once with `gitops run` |

Rig keeps both ways, with one stack schema:

| Later | From | Form in the plan |
| --- | --- | --- |
| a stack in the plan | local file | a `rig` section with the content of a `stack.hcl` (registries, trust, modules, values) |
| a stack directory | local files | `rig { stack = "./stacks/platform" }`: a directory next to the plan, with its own modules, values file and lock |
| a stack from a repository | a Git repository, or the global registry | a binding as today: provider, URL, branch or ref, subpath, `verify` keys, webhook; or `source = "acme/data-platform"` with a version |

- The same stack can be applied by the deployer and by `rig` alone; the
  deployer adds nothing to the language.
- What the binding gives today stays: fetch into memory, signed commit or
  tag, deploy on push, the description of the source in the revision.
- These three forms are decided (question 10).
- "Unchanged" comes from the releases of the cluster (section 9.3), so the
  marker files of the deployer go, and a changed ref or a removed entry is
  seen (both are missed today).
- Until the server embeds Rig, the development clone of the deployer runs
  the stack through the standalone `rig`; afterwards it sends the stack to
  the server (question 12).

| Repository (local) | Content | Later |
| --- | --- | --- |
| `norca-rig` | the library, the `rig` CLI, built-in modules, this design, the documentation site in `doc/` (Hugo, Hextra) | stays its own repository; norca and the deployer import the library |
| `norca-rig-registry` | modules converted from the packs, examples, tests, CI scripts | merged into norca-registry |
| `norca-global-registry` | the global registry (section 7.3): namespaces, keys, generated entries, the checks; `github.com/bacanoo-io/norca-global-registry` | filled from phase 6 on |
| `norca-rig-deployer` | the deployer with a `rig:` section next to `packs:`; during development it runs the stack through the standalone `rig` | merged into norca-deployer, in the form the final decision gives (question 12) |

- The two development repositories are **local clones** of norca-registry and
  norca-deployer under the other name, with the work on a branch (`rig`).
  They share history with the originals, so the later merge is an ordinary
  branch merge (fetch from the local clone, review, merge), and upstream
  changes can be pulled in meanwhile. The originals are not touched.
- In the registry clone the modules live in `modules/` next to `packs/`, so
  both can be checked in one tree until the packs go.
- The global registry repository comes after the first modules are published
  (section 7.3).
- Rig is tested against a Norca built from `main`: a dev agent for the unit
  of work, the lab for a stack (a new lab case when the first stack exists).
- Go path of the library and license (decided, question 9):
  `github.com/bacanoo-io/norca-rig`, MPL-2.0. The code is written new: nothing is copied from the
  pack manager (parts of which descend from nomad-pack), so Rig starts with
  its own history and notices only.

### 13.2 The documentation site

`doc/` in this repository is a Hugo site on the **Hextra** theme, set up as
norca-doc is (Hextra as a Hugo module, `hugo.yaml`, `content/docs/`), so the
two sites look and are built alike. It holds the user documentation of Rig:
writing a module, the language, file names, stacks, registries and the
global registry, signing and trust, the CLI, the library interface. The
design stays in `design/` and is not published there, as with norca.

## 14. From packs to modules

- Converted by hand, one pack at a time, simplest first (redis, test-app,
  rustfs), the heavy ones last (nats, consul, traefik, loadbalancer-operator).
- `rig convert pack <dir>` does the mechanical part: `metadata.hcl` and
  `variables.hcl` almost unchanged, every template file renamed without its
  `.tpl` with the template left as a comment. The template logic is rewritten
  by a person.
- A converted module is accepted when, for every example of the pack, the job
  it produces equals the job the pack renders (compared as parsed jobs, not as
  text). The registry's `check-packs.sh` already renders every example; the
  same list drives the comparison.
- What this conversion must prove is in question 2.

### 14.1 What the conversion on paper showed (phase 0, 2026-11-18)

**redis** (the built-in pack 0.2.1 of norca) is converted whole:
`examples/registry/modules/redis`. Its job file was decoded with Norca's
decoder through `DecodeJob` (with and without persistence): the job is the
one the pack renders. **nats** (norca-registry, pack 0.7.0: 63 variables, a
527-line job template, 154 lines of helpers) was gone through construct by
construct against `examples/registry/modules/nats`, which shows the hard
parts in a smaller module.

| In the packs | In Rig | Section 4 as it is |
| --- | --- | --- |
| `[[ if ]]` around an attribute (`region` when set, `static` when the port is not 0, `host_network`, `args` with persistence) | `x = cond ? v : null`: a null attribute is left out (checked with the decoder for `config`) | covers |
| `[[ if ]]` around a block (`ephemeral_disk`, `volume`, `volume_mount`, `artifact`, `separate`, `disruption`, a service, the metrics task) | `dynamic "x" { for_each = cond ? [1] : [] }`; about 14 in nats | covers, verbosely |
| the driver switch: `image` and `ports` for docker, `command` for exec | conditional attributes of `config`, null where they do not apply | covers |
| helpers (`nats.location` six times over the packs, `nats.port`, `nats.list`, `nats.permissions`, `nats.userpass`) | locals, a library child module (`lib/placement`), `natsconf` (the escaping that `nats.list` worked around is the encoder's) | covers |
| `namespace` and `region` variables in every pack | the release's: `rig.namespace`, `rig.region` (section 6.4) | covers; the variables go |
| the checks of `nats.validate` (11 `fail`) | `one_of` (driver, storage, auth_mode), `unique_by` (users), `min` (count), six `rule` blocks | covers (section 6.2 confirmed) |
| the options of a choice in `ui` only (redis `mode`, `network_mode`) | `validation { one_of }`; the form takes the options from it, `ui` keeps their labels | new rule: a value outside the options was accepted before |
| `secret { path, item }` of a variable written into a Variable item (redis password) | a `sensitive` variable taken by the item | refined in section 6.3 |
| `nomadVar` items at run time (TLS material, `client_token`) | `variable_ref()` | missing: added to section 4.2 |
| `nomadSecret` items at run time, failing when missing, trimmed | `secret_ref(..., { trim, quote })` | covers |
| `nomadService` routes without the own allocation | `service_addresses(..., { exclude_self })` | covers; the encoder must take a list that mixes known items and the runtime list (the own route first) |
| `auth_mode = "nkey"`: a Variable item split and looped over at run time; `"config"`: an item inserted as raw configuration | a task template written by hand for that part, appended to the encoded one: `data = "${natsconf(local.config)}\n${local.auth}"`, where `local.auth` is a heredoc with `{{ }}` and install-time `${}` (NATS takes top-level blocks in any order) | covers (section 4.2: a task template may still be written by hand) |
| `extra_config` indented by 10 | appended after the encoded configuration, no indentation | covers |
| `meta "pack.name"` as the default job name; `printf "nomad/jobs/%s"` | `rig.release`, `coalesce()` | covers |
| `outputs.tpl` with `if` and `range` | a heredoc output with `%{ if }`, `%{ for }` | covers |
| `job [[ var "job_name" ]]`: the ID from a variable | the label is the module's name; the ID is `id` | was wrong in the examples: section 4.5 now says it |

What the packs do wrong, which the conversion keeps (to render the same job)
and a module version of its own then fixes: redis `mode`, `service_tags` and
`extra_env` change nothing in the job, the password goes into the Variable
but Redis does not read it, and the path of the Variable ignores `job_name`.
Two of them are of a kind `rig module validate` finds by itself: **a variable
that no expression of the module reads is a warning.**

Result for phase 0: the language of section 4 holds for both packs. Added:
`variable_ref()`, the `id` of a job, a sensitive variable as the input of a
secret item, `one_of` as the source of a form's options, the warning on an
unused variable, and the encoder's lists with runtime parts. The examples
follow (`id` on every job; a Variable in Norca's format, `items { }`). Parsing
every file of `examples/` and every `hcl` block of this design found one more
fault of the drafts: a block written on one line takes one argument only
(`validation { min = 1, integer = true }` is not HCL); they are written out.

Rendering the examples through Norca's decoder and checking the jobs with
the server's `/v1/validate/job` (phase 1, 2026-11-18) corrected the drafts
once more: `disruption` is a block of a group with a string limit (`"1"`,
`"25%"`); the native service provider checks `tcp` and `http` only (a
`script` check needs Consul), so postgres checks `tcp` as its pack does;
`PGHOST` takes the host without the port; and `coalesce()` skips empty
strings as authors expect from other HCL tools (the HCL standard library's
skips nulls only). The secret lookup of a task template is `norcaSecret`
since 2026-11-14 (`nomadSecret` stays as an alias).

## 15. Phases

| Phase | Content | Result |
| --- | --- | --- |
| 0 | Spikes: decode a `job` block through a host-supplied decoder (question 4); apply evaluated resources through today's release API; convert redis and nats by hand on paper; the host interfaces of section 12 written down as Go | the language of section 4 and the library interface confirmed or corrected |
| 1 | Library core behind the library interface: load, schemas, variables with validation and rules, evaluation, `module validate/render/test`, `fmt`; local sources only; the CLI as its first host | modules can be written and tested |
| 2 | Releases: `install`, `stack plan/apply/status/destroy`, dependencies, digests, prune; against a dev agent | a stack runs on a cluster |
| 3 | Registries: git, directory, built-in, versions and tags, cache, lock; `norca-rig-registry` with the first converted modules | versions and reproducible installs |
| 4 | Supply chain: digest, sign, verify, trust, bundle, OCI | air-gapped and signed |
| 5 | The rest of the modules converted; lab case; `norca-rig-deployer` with the `rig:` section | parity with the packs of today |
| 6 | The global registry repository: namespaces, keys, generated entries, the checks of entries and of stacks; publish workflow; icons in the UI | public catalog |
| 7 | Into the products: the server embeds the library (server-side evaluation, install form), `DecodeJob` moves into norca's `jobspec2` and the copy in `cli/internal/jobspec2` is deleted (question 4), the deployer and registry branches merged, the pack manager removed | one package manager |

Phase 0 is done (2026-11-18): the ports of section 12.2 as Go, `rig.New`
with functional options, the log as a tree (section 9.5), the `rig` program
as the first host, `DecodeJob` and the job decoder (question 4), the release
API spike (section 12.2), and redis and nats on paper (section 14.1).

Phase 1 so far: load and schemas, values with validation and rules,
`module validate` (which renders the example values); evaluation (facts,
locals and child modules in the order of their references, checks, the
module's functions, runtime references resolved where they land, resources
with `enabled`/`for_each`/`count`, hooks, sensitive values, outputs) through
`Engine.Evaluate`; `natsconf`; and `module render` with Norca's job decoder.
`module test`; `fmt`; the warning on an unused variable in `module validate`;
and the encoders `jsonencode`, `yamlencode`, `tomlencode`, `natsconf`, all
Rig's own: an object's attributes by name, every string double-quoted and
escaped, a runtime value quoted for where it sits, the addresses of
`service_addresses()` as the last items of an inline list (so that an empty
catalog still gives a list). `yamlencode` writes block style, `tomlencode`
the values of a table before its subtables and a list of objects as an
array of tables; JSON and YAML keep null, NATS and TOML leave it out. Phase 1
is done but for a schema per resource kind (other kinds than jobs are the
value of their block as JSON until their kind is implemented).

Phase 2 as built (2026-11-18), checked against a dev agent
(`make integration`):

- **A stack** is `stack.hcl` (one `rig` block with `name`, `region`,
  `namespace`; `module` blocks), `variables.hcl`, `locals.hcl`,
  `outputs.hcl`, `secrets.hcl`, `values/*.vars.hcl` and its own modules
  under `modules/`; other files are refused. A release is the label of a
  `module` block; its settings are `atomic`, `wait` (default true: the next
  release waits until this one is ready; false for one that need not),
  `timeout`, `max_history`. Locals and releases are evaluated in the order
  of their references and `depends_on`. Phase 2 takes local sources only.
- **The digest of a release** is the SHA-256 of the module's tree digest
  (over its files and those of its local children, so the open point of
  6.5 is closed), the release, the region, the values (a sensitive value as
  its hash) and the settings. It is recorded in `ValuesHash`; an equal
  digest is `unchanged` and is not applied again.
- **Generated values** (`!generate:<name>` in values and in the items of a
  secret): one secret per stack, `rig/stacks/<stack>/generated` in the
  stack's namespace, created at the first apply that needs it and the only
  record of them; a generated value is sensitive. The encrypted local copy
  of 12.6 is left out: the cluster's secret store is the source.
- **The other kinds** are mapped by the host onto Norca's API type of the
  kind by its `hcl` tags, else its field names; a `name`, `path` or `id`
  left out is the resource's name, a namespace left out the release's.
- **Commands:** `rig stack validate|plan|apply|status|destroy`, `rig install
  <dir> -name`, `rig release list|status|history|rollback|uninstall`; the
  cluster is that of `NORCA_ADDR` / `NORCA_TOKEN`. `plan` exits with 2 on
  changes; `apply -prune` uninstalls what the stack owns and no longer has;
  `destroy` uninstalls in the reverse order of the stack and keeps the
  secrets.
- Left for later phases: `requires` of modules checked by Rig (the server
  checks `norca` and `features`), a saved plan (`-out`), the journal of
  12.6, the multi-region install.

Phase 3 as built (2026-11-18):

- **Addresses** (package `source`): `<registry>/<name>` is `modules/<name>`
  of a registry of the stack (`rig { registry "<name>" { source } }`),
  tagged `<name>/v<version>`; a git address (`<host>/<path>[//<dir>]`,
  `https://`, `file://`, an absolute path) tagged `<dir>/v<version>` or
  `v<version>`; `builtin/<name>`; `dir://<path>`. Any other `<a>/<b>` is the
  global registry, refused until phase 6. OCI and bundles come with phase 4.
- **Versions** (package `version`, Rig's own): semver with pre-releases,
  constraints `=`, `!=`, `>`, `>=`, `<`, `<=`, `~>` joined with commas;
  `~> 1.2` is `< 2.0.0`, `~> 1.2.3` is `< 1.3.0`; a pre-release meets only a
  constraint that names one.
- **The fetcher of the rig program** keeps a bare mirror of each repository
  and the tree of each commit in the cache (`RIG_CACHE_DIR`, else the
  user's cache directory), through the `git` program; the tree of the whole
  repository is fetched, so that a child module of the same repository
  (`../lib/placement`) is found. The built-in modules are embedded in the
  program (`builtin/redis` for now; the audit base of 12.8 later).
- **The lock file** `rig.lock.hcl`: per source the version, the constraint,
  the repository and path, the tag, its commit and the tree digest.
  `rig stack sync` writes it, keeping a locked version that still meets the
  constraint unless `-upgrade`; every other command loads the modules of
  sources as it records them and refuses a source missing from it, a
  version that no longer meets the constraint, a moved tag and content of
  another digest. The revision records the address and the tag of the
  module (`Pack.Source`, `Pack.Ref`).
- `norca-rig-registry` (next to this repository, a clone of norca-registry
  with the work on the branch `rig`) held redis, nats, postgres and
  lib/placement, tagged, with `scripts/check-modules.sh` for its CI; phase 5
  converted every pack (below).

Phase 4 as built (2026-11-18), signatures and trust:

- **Keys and statements** (package `sign`): ECDSA P-256 keys in the format
  of cosign (the private key encrypted with scrypt and nacl/secretbox under
  `RIG_KEY_PASSWORD`), key IDs and the simple signing payload as Norca's
  admission computes and reads them. `rig key generate|sign|verify`.
- **Where a signature lives:** in the module, under `.rig-signatures/`, one
  file per key. A hidden directory is outside the tree digest, so the
  signature goes with the module from every source (git, a directory, a
  bundle) without changing what it signs; it is committed before the tag.
- **Trust:** `trust { registry, require_signature, public_keys }` in the
  `rig` block of a stack, the rule of a registry or, without `registry`,
  the default. Checked at `sync` and at every load from the lock; the lock
  records the signers, which must still sign. A built-in module is part of
  the program and needs no signature. The request of a release carries the
  signatures (`Pack.Signatures`), so the admission of the server checks
  them against its `trusted_keys` (checked against a dev agent).
- A tag whose version is not the version of the module's metadata is
  refused.
- **Bundles** (`rig bundle create|inspect|import`): a reproducible archive
  (sorted entries, no times, owners or modes of the machine) of the modules
  of a stack from git sources as its lock pins them: a manifest with
  source, version, tag, commit, digest and signers, and the files of the
  directories of each module and its children, signatures included. An
  import writes them into the cache, apart from the full trees, and checks
  each digest; the fetcher takes the cached files of the locked commit when
  the repository can't be reached. A bundle is therefore not an address of
  its own: the lock file says what to install, the bundle only brings it.
  Built-in modules and directories need no bundle.
- **OCI** (`oci://<host>/<repository>`): a module version is an OCI image
  manifest of the artifact type `application/vnd.norca.rig.module.v1`,
  tagged with the version, with one layer: the tar.gz of the directories of
  the module and its children, signatures included (so a signature is not
  a cosign artifact of its own but goes with the module, as in git), and
  the module's name, version, path and tree digest as annotations. The lock
  records the manifest digest in place of a commit, so a tag pushed again
  is refused. `rig module publish [dir] -to oci://...` pushes. The client is
  Rig's own over the distribution API: anonymous, or Basic and Bearer with
  `RIG_OCI_USERNAME` / `RIG_OCI_PASSWORD`; http for localhost only. The
  credentials of docker's configuration and `rig login` are left for later
  (8.4).

Phases 1 to 4 touch only `norca-rig`. Nothing of norca, the deployer or the
registry changes before phase 5 (clones) and phase 7 (the real merge).

Phase 5 as built (2026-11-18):

- **`rig convert pack <dir> -out <dir> [-examples <dir>]`** does the
  mechanical part of section 14: the `module` block from `app` and `pack`;
  the variables without `namespace` and `region`, their checks as Rig takes
  them (`item_min_length` and `item_pattern` in an `item` block, the
  `options` of the form as `one_of`, `item_options` as `one_of` of the
  attributes of the items); the examples as `examples/<name>.vars.hcl`;
  every template kept as the comment of the file it becomes.
- **Every pack of norca-registry is a module** of norca-rig-registry
  (branch `rig`, tagged `<module>/v<version>`): consul, forgejo,
  loadbalancer-operator, mtls-gateway, nats, nodescaler-operator,
  norca-acme, norca-dns, norca-provider, openobserve, otel-collector,
  postgres, rustfs, seaweedfs, seaweedfs-csi and traefik. The earlier
  smaller nats and postgres are kept as `nats-demo` and `postgres-demo`;
  the full ones are versions 2.0.0 and 0.4.0, after the tags of the
  demonstrations.
- **The acceptance of section 14 is a script**, `compare-packs.sh` of the
  registry: for the defaults and every example, the module renders the
  jobs, ACL policies, object kinds and admission policies of the pack,
  compared as parsed resources (the pack's resource files go through rig in
  a module of their own, so both pass the same decoders). All 85 cases
  match. `check-modules-server.sh` sends every rendered job (104) to
  `/v1/validate/job` of a server; all pass.
- What the conversion showed about the language, and changed in it:
  - A null attribute is now left out of a job at every depth, not only in
    `config`: a typed field refused null, and a null duration made the
    decoder panic. The dynamic blocks of a job are expanded before, so an
    attribute of their content is evaluated with its iterator.
  - An object given where a block without labels is taken is that block:
    the `env` of a `secret` block, with names the module computes.
  - `rig module validate` evaluates the defaults too when every variable
    has one; it evaluated the examples only.
  - `strlen` is among the functions (the rules of a pack used it).
  - **Object kinds and objects are decoded by Norca's parser.** The value
    of a block mapped by field names lost the order of labelled blocks
    (the columns of a kind) and did not map `rule` blocks onto `Rules`; a
    kind did not apply. A host may decode some kinds from the block written
    as HCL in the order of the module (`host.ResourceDecoder`); the rig
    program decodes kinds and objects with a copy of norca's
    `resourcespec/object.go`, compared by `check-copy` like jobspec2, until
    phase 7.
  - Flush heredocs are indented for reading only where no line starts with
    a `%{~ }` directive: such configuration files are built from lists of
    lines (`join("\n", concat(...))`), which keeps them line for line.
  - A file named by its block's label (4.5) and a resource named by a
    `name` attribute where Norca names it otherwise (a kind's `Kind`, a
    policy's name).
- **`norca-rig-deployer`** (a clone of norca-deployer, branch `rig`) has
  the `rig:` section of 13.1: a stack directory or `stack_hcl`, values
  files, variables, `prune`, the rig program. `apply` runs `rig stack
  apply` after the packs against the cluster's API served on a local port,
  each request sent to the first server through `norca operator api`;
  checked against a dev agent (install, unchanged, destroy).
- The lab case of a stack is not written yet: norca-lab runs on the
  Hyper-V host (`run-tests.ps1`, the elevated labrunner), and a case needs
  rig built for Windows next to the deployer and the registry reached from
  there.
- Open: a variable that only rules read is reported as unused (14.1), also
  when it is a gate the installer must set (`consul_license_accepted`,
  `allow_single_datacenter`).

## 16. Open questions for the maintainer

1. **Names.** Decided by the maintainer (2026-11-15): the package is a
   *module*, the tool is *rig*. The official repository is the global
   registry of section 7.3: users' modules, and stacks that build on it.
2. **How strict is "no templates"?** Decided by the maintainer (2026-11-15):
   **the template syntax of HCL itself is allowed** (`${}`, `%{ if }`,
   `%{ for }` in heredocs and in files read with `templatefile()`). What goes
   is Go templates. Encoders stay the preferred way for configuration that is
   data (section 4.3).
3. **Version units.** Decided by the maintainer (2026-11-15): **per-module
   (and per-stack) versions and tags only**; no versions of a whole registry.
   A tested set of module versions is a published stack with its lock.
4. **The `job` block.** This draft gives the decoding to the host (section
   12.2): the core has no Norca dependency, the CLI's host package links
   Norca's job decoder, the server uses its own. Open is only whether that
   decoder can take install-time values from outside as it is, or needs a
   small change in norca when the server embeds Rig (phase 7). The
   alternative, a decoder of Rig's own against the API structs, is a second
   job language to keep in step and is not recommended.
   **Answered by the spike (2026-11-17): not as it is; a small change is
   enough.** `jobspec2.ParseWithConfig` takes text and takes values only for
   `variable` blocks of the same file, and it turns an unknown namespace
   such as `rig.release` silently into the text `"${rig.release}"`. An
   exported entry point of about 25 lines that runs the decoding of
   `jobspec2` with a context from outside,
   `DecodeJob(block *hcl.Block, ctx *hcl.EvalContext) (*api.Job, hcl.Diagnostics)`,
   decodes a job file of a module correctly: values of `var`, `local` and
   `rig`, a function of Rig, a `dynamic` block, and the runtime references
   `${attr.kernel.name}`, `${node.class}` and `${NOMAD_ALLOC_DIR}` passed
   through. It changes nothing for the callers of today.
   **Decided by the maintainer (2026-11-17): the change goes into norca in
   phase 7; until then it is part of this repository.** `cli/internal/jobspec2`
   holds the non-test files of norca's `jobspec2`, unchanged, and Rig's
   `DecodeJob` next to them (`SOURCE.md` names the norca commit);
   `cli/norcaapi` builds the `JobDecoder` on it. `make check-copy` fails when
   the copy differs from the norca checkout. In phase 7 `DecodeJob` moves
   into norca and the copy is deleted. The rig program imports norca for
   this (the CLI module only, never the core): the main module of norca
   for `jobspec2`, its `api` module for the HTTP client, with norca's own
   replacements repeated in `cli/go.mod`.
5. **Keyless signing.** Decided (2026-11-15): **keys first**; Sigstore later
   as a second signer type.
6. **Reading the cluster** during evaluation (`data` blocks). Decided
   (2026-11-15): **not in the first version**; evaluation stays a function
   of the module, the values and the install facts.
7. **Extensions outside the binary.** Decided (2026-11-15): **compile-time
   registration only**; no plugin protocol now.
8. **Icon format.** Decided (2026-11-15): **PNG 64x64 only.**
9. **Go path and license** of the library. Decided (2026-11-15):
   `github.com/bacanoo-io/norca-rig`, MPL-2.0.
10. **Stack and deployer.** Decided (2026-11-15): the deployer works from
    local files and from repositories today (checked, section 13.1), and Rig
    in the deployer keeps both, in the three forms of section 13.1 with one
    stack schema: a stack written in the plan, a stack directory next to the
    plan, a stack from a repository or the global registry.
11. **Where the first evaluation runs for the UI.** Until the server embeds
    Rig, the UI install form can only install packs. Is that acceptable for
    the development period?
12. **Standalone, embedded, or both** (decided at the end, with both built):
    does the external `rig` stay as a full program with its local data
    (section 12.6), become a thin client of the server, or go? And does the
    deployer send its `rig:` section to the server, or run it through the
    standalone program?
13. **Stacks inside stacks.** Decided by the maintainer (2026-11-15): **one
    level.** A local stack may take registry stacks as parts (section 7.5);
    a stack published in a registry uses modules only and includes no other
    stack. `rig stack validate` and `publish` refuse a registry stack with a
    `stack` part, and a part of a local stack that itself has parts.
14. **What the global registry holds.** Decided (2026-11-15): **entries**
    that point to the authors' repositories, as the OpenTofu registry, plus
    the tree digest and the signature of every version. The code of the
    modules is not copied into it.
15. **Bad versions in the global registry.** Decided (2026-11-15), following
    the OpenTofu policy: (a) removal only in the four exceptional cases, with
    a recorded decision; (b) one list of advisories and blocked versions with
    reasons next to the entries; (c) getting listed through an issue form
    and a review.

Open after 2026-11-17: question 11 (a consequence of the development
order), question 12 (decided at the end).
Without a phase yet: evaluation on an observer, apply on the leader (section
12.7); the audit base module, which sits on the internal bus once Rig is
embedded (section 12.8).
