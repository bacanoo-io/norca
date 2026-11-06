<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Server-Side Pack Registry, Application Catalog, and Schema-Driven Web UI Architecture

## 1. Summary

Norca's package manager (`norca pack`) and server-side release engine (`packs.norca.io/Release`, see [package-manager-and-releases.md](package-manager-and-releases.md)) keep releases as Raft-backed revisions with ordered resources, atomic rollback and drift detection. Registries and template rendering were client-side only: `norca pack registry add` caches packs on a workstation, and the Web UI could only edit the values of a release as JSON and print a CLI command.

This document describes how the **cluster pack registries**, the **catalog**, the **server-side render** and the **install form** work: operators add registries in the UI or API, browse their packs, and install or upgrade them with a form built from the pack's own `variables.hcl`. It describes the implementation; section 9 lists what is still open.

| Capability | Before | Now |
| :--- | :--- | :--- |
| Installed releases (`/ui/packages`) | Release list with phase, drift and signature | Tabs **Installed Releases**, **Catalog**, **Registries** |
| Release upgrade (`/ui/packages/:ns/:name`) | JSON values editor that prints a CLI command | **Upgrade** opens the install form with the values of the deployed revision (releases from a cluster registry); **Edit values** keeps the JSON editor and CLI command |
| Registries | Workstation cache only | Cluster objects `packs.norca.io/Registry`, managed in the UI and API, synced on request, on an interval and from NATS |
| Catalog | None | `GET /v1/packs`: the packs of all registries with version, category, signature and compatibility |
| Rendering | Client only | The leader renders packs of cluster registries and plans or applies them as server-rendered revisions |
| Parameters | JSON textarea | Form with tabs, groups, widgets, conditional fields, validation, Variable secrets |

---

## 2. Architecture

```
+---------------------------------------------------------------------------------------------+
|                               NORCA WEB UI (/ui/packages)                                   |
|  Registries tab              Catalog tab                     Installed Releases / release   |
|  (Packages::Registries)      (Packages::Catalog)             (Packages::List / ::Release)   |
|  - add / edit / sync /       - search, category, registry    - Upgrade (cluster registry)   |
|    remove, sync interval     - README, Install               - Edit values (JSON + CLI)     |
|                 \                     |                            /                         |
|                  \          Packages::SchemaForm (+ ::SchemaField, utils/pack-form.js)       |
|                   \         tabs, groups, widgets, show-if, validation, Plan, Install        |
+--------------------\-----------------|--------------------------/---------------------------+
                      \   HTTP: /v1/pack-registr*, /v1/packs, /v1/pack/..., /plan-pack, /deploy-pack
                       v               v                         v
+---------------------------------------------------------------------------------------------+
|  ANY AGENT (client or server): command/agent/pack_endpoint.go -> RPC Packs.*                  |
|  FOLLOWER: forwards Packs.Upsert/Sync/Delete/Detail/Deploy to the leader                     |
+---------------------------------------------------------------------------------------------+
|  LEADER (norca/norca/pack_registry.go, pack_registry_endpoint.go)                            |
|  - packs.norca.io/Registry objects in Raft (status: packs, generation, last good sync)       |
|  - fetches registries into <data_dir>/packs/<registry>/ (staging, then swap)                 |
|  - periodic sync loop (spec.syncInterval)                                                    |
|  - form schema synthesis (pack/catalog, pack/sdk/pack/variables/schema.go)                   |
|  - render (pack/catalog.RenderPack) -> Release.Apply as a server-rendered revision           |
+---------------------------------------------------------------------------------------------+
        ^ POST /v1/pack-registry/:name/sync (API client)
        |
  norca-natsbridge -sync-pushes / -sync-regions  <---  NATS  <---  CI: norca-natsbridge notify-push
```

---

## 3. Cluster Pack Registries

### 3.1 Object model (`packs.norca.io/Registry`)

Registries are cluster-scoped custom objects of a region (`norca/norca/pack_registry.go`). Spec and status keep the camelCase keys of the Go types:

```json
{
  "Name": "norca",
  "Spec": {
    "type": "git",
    "source": "https://github.com/bacanoo-io/norca-registry",
    "defaultRef": "v0.1.0",
    "syncInterval": "1h",
    "trustedKeys": ["-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----"]
  },
  "Status": {
    "phase": "Ready",
    "message": "Synchronized 9 pack(s) with Norca 0.1.0",
    "lastSyncedAt": "2026-10-29T08:00:00Z",
    "lastGoodSyncAt": "2026-10-29T08:00:00Z",
    "syncGeneration": 12,
    "packCount": 9,
    "packs": [
      {
        "Registry": "norca", "Name": "postgres", "Version": "0.3.0",
        "Description": "...", "Category": "database",
        "Digest": "sha256:...", "Signed": true, "Signer": "...",
        "Requires": {"norca": ">= 0.1.0", "features": ["releases"]},
        "Compatible": true, "HasSchema": true
      }
    ]
  }
}
```

| Field | Meaning |
| :--- | :--- |
| `spec.type` | `oci`, `git`, `dir` (a directory on the servers; source `builtin` is the packs that ship with Norca), `bundle` (an offline bundle on the servers) or `uploads` (uploaded packs, section 3.5). |
| `spec.source` | `oci://…`, a Git URL, a server path, or `builtin`. |
| `spec.defaultRef` | Tag or branch to fetch (`latest`). |
| `spec.syncInterval` | Periodic sync on the leader, at least `1m`; empty or `0s` syncs on request only. |
| `spec.trustedKeys` | Signing keys; with keys, only packs signed by them can be installed. |
| `spec.authSecret` | `{namespace, path}` of a Variable with the credentials of a private git or OCI source (section 3.4). |
| `status` | `phase` (`Ready`/`Error`), `message`, `lastSyncedAt` (the last attempt), `lastGoodSyncAt`, `syncGeneration`, `packCount` and the indexed `packs`, one entry per pack version. |

### 3.2 Leader, cache and generations

- **Every pack-content operation runs on the leader.** The HTTP handlers call the `Packs.*` RPC endpoint (`norca/norca/pack_registry_endpoint.go`): `List` and `Catalog` read the replicated state anywhere; `Upsert`, `Sync`, `Delete`, `Detail` and `Deploy` are forwarded to the leader. Client agents work too.
- **Cache.** The leader fetches a registry into `<data_dir>/packs/<registry>/<ref>/<pack>@<ref>/`. A sync fetches into a staging directory and replaces the cache only when it succeeded; a failed sync records `Error` with the message and keeps the packs of the last good sync (`lastGoodSyncAt`), and the cache keeps serving them.
- **Generations.** Every successful sync increments `status.syncGeneration` and writes it into the cache (`.generation`). A server whose cache was fetched for another generation, such as a new leader, syncs the registry again before it serves a pack.
- **Compatibility.** `GET /v1/packs` and the pack detail evaluate each pack's `requires { norca, features }` against the servers and return `Compatible` and `MissingFeatures`; the UI blocks the install of an incompatible version.

### 3.3 Syncing

| Trigger | How |
| :--- | :--- |
| Save | Creating or updating a registry syncs it before the request returns. |
| Request | `POST /v1/pack-registry/:name/sync`, **Sync** in the UI. |
| Interval | The leader checks every 30s for registries whose `syncInterval` passed since `lastSyncedAt`. |
| Push notice | `norca-natsbridge -sync-pushes` subscribes to `<prefix>.packs.pushed` (queue group per region) and syncs the registries of the source a notice names; CI publishes it with `norca-natsbridge notify-push -source <url>` (the release workflow of the norca-registry repository does). |
| Other regions | `norca-natsbridge -sync-regions` forwards the registry events and syncs a local registry when the one of the same name, type and source in another region found other packs (versions and digests). |

NATS stays an accelerator: nothing in the cluster reads from it, the interval sync covers a NATS outage, and `-sync-cooldown` (default `1m`) limits the syncs a registry gets from NATS (`norca/natsbridge/README.md`).

### 3.4 Private registries

Private git and OCI sources take their credentials from a Variable named by `spec.authSecret` (`namespace`, `path`): the items `username` and `password` (or `token`) for `https://` git and OCI sources, `ssh_private_key` and `known_hosts` for git ssh sources. Saving a registry with an auth secret needs read access to that Variable, on every save, so that someone who can only write registries can't point a source at a host of their choosing and receive the credentials of others. The leader reads the Variable for every sync and sends the credentials to the host of the source only: OCI requests to other hosts (a token service elsewhere, redirects) carry no Authorization header, git gets them as a header for the source URL without following redirects, and ssh checks the host key against `known_hosts`. The credentials never reach a URL, the clone, the cache or the status, and the servers never use credentials of their own environment or docker configuration.

| Source | Items | How the credentials are sent |
| :--- | :--- | :--- |
| OCI | `username` + `password`, or `token` | Basic or token auth to the registry host only (`oci.Options.AuthHost`); requests to other hosts lose the Authorization header |
| git `https://` | `password` or `token`, optional `username` (default `git`) | `http.<source host>.extraHeader` for one `git clone`, `http.followRedirects=false`, no credential helpers, no prompts (`caching.GitAuth`) |
| git ssh (`ssh://`, `user@host:path`) | `ssh_private_key`, `known_hosts` | A temporary key file, `StrictHostKeyChecking=yes` against `known_hosts`, no agent |

A token service on another host than the registry (such as Docker Hub's) therefore gets no credentials.

### 3.5 Uploaded packs

Packs without a registry are uploaded (`PUT /v1/pack-upload`, RPC `Packs.Upload`) to a registry of the type `uploads`, created by the first upload. An upload (at most 8 MiB) is a bundle, verified with the registry's trusted keys, or a tar or tar.gz of pack directories, refused by a registry with trusted keys (`pack/catalog/upload.go`: no absolute names, no `..`, no links, at most 64 MiB and 4096 files extracted). The archive is checked by importing it into a temporary cache, then stored in Raft as chunks of 160 KiB, objects of the kind `packs.norca.io/PackUpload` (`norca/norca/pack_upload.go`): every leader rebuilds the registry from them, and backups include them. A sync imports the uploads, oldest first, into the cache under `<version>/<pack>@<version>`, and lists them in `status.uploads`; removing an upload deletes its chunks. Releases installed from a local directory or a bundle (`pack.source` empty or `bundle:...`) are upgraded from an `uploads` registry that has the pack.

---

## 4. The Install Form (`UIFormSchema`)

`GET /v1/pack/:registry/:pack[?version=]` returns the pack entry, its versions, its README and the `UIFormSchema`: a Draft 2020-12 JSON Schema with `x-ui-*` directives, synthesized by the leader from the pack (`catalog.SynthesizeFormSchema`, `pack/sdk/pack/variables/schema.go`).

### 4.1 Declared in HCL

The form is declared next to the variables; pack authors don't write JSON.

`metadata.hcl` declares the tabs, in their order:

```hcl
ui {
  tab "general" {
    label = "General"
  }
  tab "security" {
    label = "Security & Secrets"
    icon  = "lock"
  }
}
```

A variable may have `ui`, `secret` and `validation` blocks (`pack/internal/pkg/variable/decoder/ui.go`), and `rule` blocks state conditions across variables (`pack/internal/pkg/variable/rules`):

```hcl
variable "mode" {
  type    = string
  default = "standalone"
  ui {
    tab           = "general"
    group         = "Topology"
    order         = 1
    options       = ["standalone", "replica", "sentinel"]
    option_labels = { replica = "Primary + Replicas", sentinel = "High Availability (Sentinel)" }
  }
}

variable "resources" {
  type    = object({ cpu = number, memory = number })
  default = { cpu = 500, memory = 256 }
  ui {
    tab = "resources"
  }
}

variable "replicas" {
  type    = number
  default = 2
  ui {
    tab     = "resources"
    widget  = "slider"
    unit    = "nodes"
    presets = [1, 3, 5]
    show_if = { field = "mode", equals = "replica" }
  }
  validation {
    integer = true
    min     = 1
    max     = 9
  }
}

variable "redis_password" {
  type    = string
  default = ""
  ui {
    tab     = "security"
    label   = "Redis Password"
    show_if = "auth_enabled"
  }
  secret {
    path = "norca/jobs/redis"   # a Variable the pack declares (*.var.hcl.tpl)
    item = "password"           # default: the variable name
  }
  validation {
    min_length = 12
  }
}

rule "resources" {
  condition     = var.resources.cpu >= 100 && var.resources.memory >= 128
  error_message = "resources: cpu must be at least 100 and memory at least 128."
}
```

| Block | Attributes |
| :--- | :--- |
| `ui` | `tab`, `group`, `order`, `label`, `widget`, `unit`, `presets`, `options` (allowed values; a choice widget unless `widget` says otherwise), `option_labels`, `show_if` (a variable name, shown when it is set, or `{ field, equals \| notEquals \| in }`) |
| `secret` | `path`, `item`: the value goes to that item of the Variable, applied by the servers but never stored in the release or rendered into the templates (section 5.3). |
| `validation` | `min`, `max`, `integer`, `pattern`, `min_length`, `max_length`, `min_items`, `max_items`, `unique`, `item_pattern`, `item_min_length` |
| `rule` (top level) | `condition` (HCL; `var.<name>`, the job specification functions plus `regex`, `regexall`, `alltrue`, `anytrue`), `error_message` |

Without a `ui` block the tab and group are inferred from the variable name (secret-like names → `secrets`, `cpu`/`memory`/`count`/`replica` → `resources`, `port`/`tls`/`service`/`host` → `network`, `volume`/`storage`/`backup` → `storage`, dependency variables → `dependencies`, else `general`), the widget from the type, and variables without a default are required. Tabs used but not declared come after the declared ones. The built-in packs (`norca/norca/pack_builtin_catalog.go`) and the packs of the [norca-registry](https://github.com/bacanoo-io/norca-registry) use only these blocks.

An explicit JSON Schema (`variables_schema = file("values.schema.json")` in `metadata.hcl`) is still accepted and laid over the synthesized schema, `x-ui-*` keys included; it then replaces the HCL constraints for validation.

### 4.2 Widgets

The UI (`norca/ui/app/utils/pack-form.js`, `Packages::SchemaField`) maps the widgets to `Ui::*` components:

| `widget` | Type | Control |
| :--- | :--- | :--- |
| `text` (default for `string`) | string | Input |
| `number` (default for numbers), `slider` | number | Number input with unit and preset buttons; `slider` (needs validation min < max, number type; `step` → `x-ui-step`) is a range input beside the number input, presets as datalist marks |
| `switch` (default for `bool`) | bool | Switch |
| `segmented` (≤ 3 options), `select` | string | Radio group / select with `option_labels` |
| `secret` | string | Masked input with show/hide; on upgrade a Variable secret left empty keeps the stored item |
| `code` | string | Monospace multi-line input |
| `tags`, `datacenter-multiselect` | list of strings or numbers | Comma or line separated text |
| `key-value` | map | `key=value` lines |
| `object-list` | list of objects | JSON |
| (object type) | object | Nested fields of its attributes |
| `namespace-select`, `region-select` | string | Select of the cluster's namespaces / regions |
| `node-pool-select`, `storage-volume-select` | string | Text input |

### 4.3 Form behaviour

- **Tabs and errors.** Each declared tab is a tab of the form, plus a **JSON** tab with the overrides as JSON, edited both ways (`applyJSON` in `utils/pack-form.js`: unknown variables and Variable secrets are refused, an invalid text blocks the install), and a **CLI command** tab with the equivalent `norca pack run` (secrets shown as `<secret>`). Validation runs on Plan and Install; a tab shows the number of its errors and the form opens the first tab that has one. Server errors (failed rules, render errors) are shown above the buttons.
- **Conditional fields.** Fields whose `show_if` does not hold are hidden and not sent.
- **Values sent.** Only the overrides: values that differ from the defaults, required ones and those the release already had. Values of CLI installs (strings of `-var`) are converted to the field types; `(redacted)` values of secret-like variables without a `secret` block must be entered again on upgrade.
- **Versions.** The form has a version select; another version reloads its schema and keeps the values that still apply. An upgrade whose version left the registry offers the versions it has.
- **Release name and namespace** are set on install and fixed on upgrade; the region is the active region of the UI.

---

### 4.4 Upgrading CLI installs

`norca pack run` records `pack.source` (the git URL of the registry from the cache's `metadata.json`, or the `oci://` location and tag from the pack's origin) and `pack.ref` in the release; the server-side render records those of the cluster registry. The release page (`utils/releases.js` `upgradeTarget`) upgrades from the cluster registry the release names if its source matches, else from a registry of the same source that has the pack. Without one, `registrySuggestion` offers to add it (name from the release's registry, type from the source, reference from `ref` or the OCI tag); packs from local directories and bundles are uploaded (section 3.5). Sources compare without the scheme case, a trailing `/` or `.git`, and for OCI without the tag.

## 5. Server-Side Render, Plan and Deploy

```
UI form: {Registry, Pack, Version, Values, Secrets, Description}
        │  POST /v1/release/:name/plan-pack | deploy-pack   (namespace, region from the query)
        ▼
RPC Packs.Deploy, forwarded to the leader
        ▼
1. Resolve the pack in the leader's cache (generation checked), verify its digest and
   signature against the registry's trusted keys
2. Parse the variables; check the validation blocks, ui options and rules
3. Move the values of variables with a secret block into Secrets (the templates see the defaults)
4. Render the templates in memory; parse the resource templates (*.var.hcl, *.acl.hcl, ...)
5. Convert to a release request (nomad.Config.ReleaseFromRendered, set by the agent)
6. Release.Apply as a server-rendered revision (DryRun for plan-pack)
```

### 5.1 Security

1. **The caller's token end to end.** `Release.Apply` authorizes and applies every resource with the caller's token, as for `norca pack run`.
2. **Server-rendered is not spoofable.** The server-rendered flag (visible to admission policies) is only set in-process on the leader; RPC callers can't claim it.
3. **Sandboxed packs.** Templates render in memory from the cached pack; `file()` in `metadata.hcl` can't leave the pack directory.

### 5.2 Validation

The CLI and the leader check the `validation` blocks, `ui` options and rules before rendering. The synthesized schema of the `validation` blocks and options is the variables schema of the revision, which the servers check again; rules are checked where the pack is rendered.

### 5.3 Secrets

- A variable with a `secret` block is a secret input: its value becomes an item of the named Variable (`ReleaseApplyRequest.Secrets`), never a release value, and the templates see its default. This holds for the UI, `norca pack run --var` and the server-side render; `--var-secret` overrides it, and an upgrade without a value keeps the stored item.
- Secret-like variables without a `secret` block are recorded as `(redacted)` in the release values but are still rendered into the templates, as with `norca pack run --var`.

---

## 6. HTTP API

| Endpoint | Method | ACL | Purpose |
| :--- | :--- | :--- | :--- |
| `/v1/pack-registries` | `GET` | `packs.norca.io/Registry` read in `namespace "*"`, or release read/write in any namespace | Registries and their status |
| `/v1/pack-registry` | `POST`, `PUT` | `object "packs.norca.io/Registry" { capabilities = ["write"] }` in `namespace "*"` | Create or update a registry and sync it |
| `/v1/pack-registry/:name` | `GET`, `PUT`, `DELETE` | read / write as above | Read, update, or remove a registry and its cache |
| `/v1/pack-registry/:name/sync` | `POST` | write as above | Sync now |
| `/v1/packs[?registry=]` | `GET` | read as above | The catalog |
| `/v1/pack/:registry/:pack[?version=]` | `GET` | read as above | Pack, versions, README, `UIFormSchema` |
| `/v1/release/:name/plan-pack` | `POST` | `packs.norca.io/Release` write + the ACLs of the resources | Render and plan |
| `/v1/release/:name/deploy-pack` | `POST` | as above | Render and apply |

---

## 7. UI Components

| Component | Role |
| :--- | :--- |
| `Packages::List` | The page with the three tabs |
| `Packages::Registries` | Registry table (status, packs, last and next sync), add/edit modal (type, source, reference, sync interval, trusted keys, built-in preset), sync, remove |
| `Packages::Catalog` | Pack cards (newest version, category, compatibility, signature), filters, side panel with Install and README |
| `Packages::SchemaForm`, `Packages::SchemaField` | The install and upgrade form |
| `Packages::Release` | Upgrade modal for releases of a cluster registry |
| `utils/pack-form.js` | Schema to form model, values, overrides, secrets, validation, CLI command |

Mirage (`norca/ui/mirage/norca/packs.js`) serves the same JSON as the servers; the acceptance tests are in `norca/ui/tests/acceptance/packages-test.js`.

---

## 8. Tests

- `norca/norca/pack_registry_endpoint_test.go`: forwarding from a follower, generation refetch, failed sync keeps the packs, interval sync.
- `norca/command/agent/pack_endpoint_test.go`: install of a built-in pack over HTTP; the secret variable is not a release value.
- `norca/pack/catalog/form_test.go`, `pack/internal/pkg/variable/decoder/ui_test.go`, `.../rules/rules_test.go`, `pack/sdk/pack/variables/schema_test.go`: HCL form directives, validation, rules, secret routing.
- `norca/natsbridge/sync_test.go`: push notices, cross-region events, queue group.

---

## 9. Open

1. A **multi-region** selection on install. (The **atomic** choice is on the form: namespace default, computed in the UI from the namespace meta like `structs.ReleaseAtomicDefault`, which sends no `Atomic`; or on/off, sent as `Atomic` and shown as `--atomic=` in the CLI tab.)
2. The Ember acceptance tests run on Linux (CI); the test build fails to load on Windows.
