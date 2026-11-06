<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Web UI Architecture, Dual-UI Embedding Pipeline, and Realtime Integration

## 1. Executive Summary & Core Architectural Principles

Norca introduces a clean-room, modern web user interface (`norca/ui/`) built on **Ember.js 6.12 (Octane)** and the **Tabler 1.6 (MIT)** design system, engineered to provide full operational visibility and administrative control over both classic Nomad primitives and all newly architected Norca subsystems (native package releases, custom objects/CRDs, built-in uBixVault secret store, CEL/webhook admission control, native service discovery with mTLS intentions, L3/L4 network policies, multi-dimensional namespace quotas, built-in autoscaling, and encrypted `age` backups).

To guarantee zero operational disruption during the migration, Norca compiles both the new primary UI (`/ui/`) and the previous UI (`/ui/legacy/`) into a single, zero-external-dependency Go binary via an in-memory virtual filesystem (`command/agent/bindata_assetfs.go`).

### Key Architectural Tenets:
1. **Clean-Room Component & Style Encapsulation**: All proprietary HashiCorp Design System (`@hashicorp/design-system-components`), Flight Icons (`@hashicorp/ember-flight-icons`), and Bulma dependencies have been stripped from the primary UI and replaced by an open-source **Tabler CSS + Tabler Icons** foundation isolated in a CSS Cascade Layer (`@layer tabler;`), topped by a strict `Ui::*` Glimmer component layer enforced at build time (`norca/ui/scripts/check-template-classes.js:1`).
2. **Zero-External-CDN Security Posture**: Every stylesheet, JavaScript bundle, SVG icon, and font metric is compiled into the binary and served from `'self'`. The HTTP server enforces a strict `Content-Security-Policy` header (`norca/structs/config/ui.go:103`) that blocks external scripts, frames, and form actions, allowing the UI to operate identically in air-gapped data centers.
3. **Dual-Engine Realtime Synchronization**: Classic Ember Data entities synchronize via long-polling blocking queries (`?index=<N>&wait=60s`), while the federation dashboard and all Norca feature domains subscribe to a multiplexed NDJSON event stream (`/v1/event/stream`) with exact Raft index resumption (`exact=true`) and automatic `410 Gone` state resynchronization (`norca/ui/app/services/event-stream.js:36`).
4. **Ephemeral Client-Side Cryptographic Hygiene**: Sensitive cryptographic inputs—such as `age` private keys used to browse or restore backups (`norca/ui/app/services/backups.js:21`)—are held strictly in volatile JavaScript tab memory (`@tracked identity`), never persisted to `localStorage`, `sessionStorage`, cookies, or URL query parameters, and purged immediately on decryption failure or tab closure. Shamir unseal shares are never entered into the web UI at all (`norca/ui/app/components/secrets/seal-status.js:16`).
5. **Single-Binary Dual-UI Transition Mount**: The Go HTTP server mounts a custom `UIAssetWrapper` (`norca/command/agent/http.go:644`) over the embedded asset tree that routes `/ui/*` client-side history paths to `index.html` while isolating `/ui/legacy/*` routes to `legacy/index.html`, backed by a cross-platform Linux/Windows extraction and embedding pipeline (`norca/scripts/legacyui/main.go:1`, `windows.bat:57`).

---

## 2. End-to-End Architecture & Request Routing

```
+---------------------------------------------------------------------------------------------------+
|                                      OPERATOR BROWSER (SPA)                                       |
|                                                                                                   |
|  +---------------------------------------------------------------------------------------------+  |
|  | Global Frame (PageLayout / FrameService: Region Scope, Namespace Scope, Theme, Ctrl+K)     |  |
|  +----------------------------------------------+----------------------------------------------+  |
|                                                 |                                                 |
|         +---------------------------------------+---------------------------------------+         |
|         |                                       |                                       |         |
|         v                                       v                                       v         |
|  +-----------------------------+  +---------------------------+  +-----------------------------+  |
|  |   Ember Data / Watchable    |  |   EventStreamService      |  |   StreamLogger / Sockets    |  |
|  |   Adapters (app/adapters/)  |  |  (services/event-stream)  |  |  (Logs & WebSocket Exec)    |  |
|  |                             |  |                           |  |                             |  |
|  |  GET /v1/jobs?index=42      |  |  GET /v1/event/stream     |  |  GET /v1/client/fs/logs     |  |
|  |  Track X-Nomad-Index        |  |  ?topic=...&index=43      |  |  WSS /v1/client/allocation/ |  |
|  |  via WatchListService       |  |  &exact=true (NDJSON)     |  |      <id>/exec?tty=true     |  |
|  +--------------+--------------+  +-------------+-------------+  +--------------+--------------+  |
+-----------------|-------------------------------|-------------------------------|-----------------+
                  | HTTPS + X-Nomad-Token         | Chunked NDJSON Stream         | Chunked / WSS
                  v                               v                               v
+---------------------------------------------------------------------------------------------------+
|                             NORCA AGENT HTTP SERVER (command/agent/http.go)                       |
|                                                                                                   |
|  +----------------------------------------------+----------------------------------------------+  |
|  |                             http.ServeMux + CompressHandler                                 |  |
|  +----------------------+-----------------------------------------------+----------------------+  |
|                         |                                               |                         |
|           Path: / or /ui/*                                      Path: /v1/*                       |
|                         |                                               |                         |
|                         v                                               v                         |
|  +----------------------------------------------+  +-------------------------------------------+  |
|  | handleUI() Middleware (http.go:693)          |  | API Wrappers: s.wrap() / s.wrapNonJSON()  |  |
|  | Injects Content-Security-Policy Header       |  | - Sets X-Nomad-Index, X-Nomad-KnownLeader |  |
|  +----------------------+-----------------------+  | - Parses ?region, ?namespace, ?wait       |  |
|                         |                          | - Enforces ACL Token & CORS / noxssrw     |  |
|                         v                          +--------------------+----------------------+  |
|  +----------------------------------------------+                       |                         |
|  | UIAssetWrapper.Open(name) (http.go:648)      |                       v                         |
|  |  ├── Exact asset match? -> Serve bytes       |  +-------------------------------------------+  |
|  |  └── os.ErrNotExist (SPA History Fallback):  |  | Norca RPC / Raft State Machine /          |  |
|  |       ├── isLegacyUIPath("legacy/*")         |  | Event Broker / Client FileSystem & Exec   |  |
|  |       |     -> Serve "legacy/index.html"     |  +-------------------------------------------+  |
|  |       └── Default                            |                                                 |
|  |             -> Serve "index.html"            |                                                 |
|  +----------------------+-----------------------+                                                 |
|                         |                                                                         |
|                         v                                                                         |
|  +----------------------------------------------+                                                 |
|  | Embedded In-Memory VFS (bindata_assetfs.go)  |                                                 |
|  | Built with `-tags ui`:                       |                                                 |
|  |  ├── dist/index.html, dist/assets/* (New UI) |                                                 |
|  |  └── dist/legacy/index.html, ... (Legacy UI) |                                                 |
|  +----------------------------------------------+                                                 |
+---------------------------------------------------------------------------------------------------+
```

---

## 3. Frontend Stack & Clean-Room Design (`norca/ui/`)

### 3.1 Ember.js Octane & Build Architecture
The primary web UI in `norca/ui/` is built on modern **Ember.js 6.12.0** (`edition: "octane"`) with **Ember CLI 6.12.0**, **Glimmer Components (`@glimmer/component` 1.1.2)**, and tracked reactivity (`@glimmer/tracking` 1.1.2) (`norca/ui/package.json:104-160`).

| Subsystem | Package & Version (`norca/ui/package.json`) | License | Architectural Role |
| :--- | :--- | :--- | :--- |
| **Core Framework** | `ember-source@6.12.0`, `@glimmer/component@^1.1.2` | MIT | Reactive component model, URL router (`locationType: 'history'`, `rootURL: '/ui/'`), and DI container. |
| **Design System** | `@tabler/core@1.6.1`, `@tabler/icons@3.49.0` | MIT | Neutral CSS foundation, layout grid, dark/light color tokens, and outline SVG icon ASTs. |
| **Charts & Graphs** | `echarts@6.1.0`, `d3-*@3.x/4.x` | Apache-2.0 / ISC | Lazy-loaded SVG charts (`Ui::Chart`) for allocation status, scaling step-lines, and mTLS intention graphs; D3 for cluster topology (`<TopoViz>`). |
| **Code & Diff** | `codemirror@5.58.2`, `jsonlint@1.6.3` | MIT | HCL/JSON job editor, custom object YAML/JSON editor, and line-by-line unified diff viewer (`Ui::TextDiff`). |
| **Interactive Shell** | `xterm@^4.6.0`, `xterm-addon-fit@0.5.0`, `anser@^2.1.1` | MIT | WebSocket PTY terminal emulation for allocation task `exec` and ANSI color stream rendering for task/agent logs. |
| **Authorization** | `ember-can@~7.0.0` + `utils/norca-acl.js` | MIT / MPL-2.0 | Declarative route/component permission gates matching server-side ACL policy evaluation (`allowsBlock`, `allowsNamespace`, `allowsSecret`, `allowsObject`). |

### 3.2 CSS Cascade Layer Isolation & Design Tokens
To prevent selector specificity wars between third-party framework styles and Norca's custom components, `norca/ui/lib/norca-styles/index.js:20` implements a custom Broccoli build plugin (`TablerLayer`) using PostCSS (`norca/ui/lib/norca-styles/scope.js:12`):

1. At build time, `TablerLayer` reads `@tabler/core/dist/css/tabler.css`, strips `@charset` declarations, and wraps the entire stylesheet inside `@layer tabler { ... }`, emitting `vendor/norca/tabler.layered.css`.
2. In CSS Cascade Layers, **unlayered styles unconditionally beat layered styles**, regardless of selector specificity.
3. Norca's application stylesheet (`norca/ui/app/styles/app.scss` $\rightarrow$ `norca.scss`) is compiled unlayered into `assets/nomad-ui.css`, guaranteeing that Norca design tokens (`norca/ui/app/styles/norca/_tokens.scss:19`) and component overrides always win cleanly without `!important` hacks.

#### Functional Design Tokens (`_tokens.scss:19-46`):
- **Region Identifier Palette**: Six deterministic region colors (`--norca-region-1` through `--norca-region-6`, mapped to Tabler's azure, purple, orange, teal, pink, and lime) provide consistent visual identity across the federation strip and multi-region tables:
  $$\text{colorIndex}(\text{region}) = \left(\text{indexOf}(\text{sortedRegions}, \text{region}) \bmod 6\right) + 1$$
- **Semantic Status Tones**: `--norca-tone-success`, `--norca-tone-info`, `--norca-tone-warning`, `--norca-tone-danger`, and `--norca-tone-neutral` map directly to `--tblr-*` status variables and adapt automatically between `[data-bs-theme="light"]` and `[data-bs-theme="dark"]`.
- **Tabular Typography & Accessibility**: `body` enforces `font-feature-settings: 'tnum' 1` so live-updating counters and metrics never jitter horizontally; `:focus-visible` enforces a `2px solid var(--tblr-primary)` outline; `@media (prefers-reduced-motion: reduce)` collapses all CSS transitions and disables ECharts animations (`norca/ui/app/utils/charts.js:63`).

### 3.3 Build-Time Template Class & Component Linter
To guarantee that legacy Bulma classes or proprietary HashiCorp components never re-enter the codebase, `norca/ui/scripts/check-template-classes.js:1` (invoked via `yarn lint:classes` during `yarn test`) statically parses every `.hbs` template across `app/templates/` and `app/components/`:
- **Forbidden Components (`check-template-classes.js:84-90`)**: Any occurrence of `<Hds::`, `<FlightIcon`, `{{x-icon`, or `{{flight-icon` fails the build immediately.
- **Strict Class Allowlist (`check-template-classes.js:35-82`)**: Outside `app/components/ui/`, `app/components/frame/`, and `app/components/page-layout.hbs`, templates may **only** use layout utilities (Bootstrap/Tabler grid `row`/`col-*`, spacing `m*-*`/`p*-*`, flex/display `d-*`/`flex-*`/`gap-*`, typography `h1`-`h6`/`text-*`/`font-monospace`) and `norca-*` semantic classes. Raw Tabler component classes (such as `card`, `btn`, `badge`, `table`, `modal`) are restricted exclusively to the `Ui::*` component layer (`app/components/ui/`).

### 3.4 Global Frame, Theme Engine, and Command Palette (`FrameService`)
Every route renders inside `<PageLayout>` (`norca/ui/app/components/page-layout.hbs:8`), coordinated by `FrameService` (`norca/ui/app/services/frame.js:49`):
- **Color Theme Engine (`frame.js:76-90`)**: Supports `'light'`, `'dark'`, and `'auto'` (persisted in `localStorage.norcaTheme`). In `'auto'` mode, `FrameService` attaches a listener to `window.matchMedia('(prefers-color-scheme: dark)')` and sets `document.documentElement.setAttribute('data-bs-theme', effectiveTheme)`. The `<Frame::Sidebar>` remains pinned to `data-bs-theme="dark"` for high-contrast navigation framing.
- **Region & Namespace Scope (`frame.js:111-169`)**: Topbar selectors switch the active region (`?region=`) or activate the federated `"All regions"` (`*`) scope (`localStorage.norcaRegionScope`) alongside the active namespace (`localStorage.norcaNamespace`). Routes that span regions (`index`, `jobs.index`, and routes with `metadata.spansRegions`) query all federated regions concurrently.
- **Global Command Palette (`components/frame/command-palette.js:27`)**: Triggered anywhere via `Ctrl+K` (`Cmd+K`) or `/` outside input fields (`frame.js:172`). Combines RBAC-filtered navigation routes, quick actions (Run a job, Toggle theme, Switch region, Open `/ui/legacy/`), and debounced (`200ms`) cluster-wide fuzzy search (`POST /v1/search/fuzzy` with `{"Text": text, "Context": "all", "Namespace": "*"}`), returning up to 5 deep-linked matches per entity category (Jobs, Clients, Allocations, Task Groups, CSI Plugins).

---

## 4. State Synchronization, Realtime Streaming & Session Management

Norca's UI employs four specialized transport mechanisms depending on the cardinality and latency requirements of each view.

```
+--------------------------------------------------------------------------------------------------+
|                        STATE SYNCHRONIZATION & REALTIME TRANSPORT MATRIX                         |
+--------------------------------------------------------------------------------------------------+

1. EMBER DATA BLOCKING QUERIES (adapters/watchable.js + services/watch-list.js)
   Browser                                                    Norca HTTP Server
     |--- GET /v1/jobs?index=1042&region=global ------------->| Blocks until Raft index > 1042
     |<-- 200 OK (X-Nomad-Index: 1055, JSON payload) ---------| or MaxQueryTime timeout
     |    WatchListService updates url -> 1055                |
     |    Diffs idsInPayload() & purges deleted store records |

2. MULTIPLEXED NDJSON EVENT STREAM (services/event-stream.js + utils/event-stream-connection.js)
   Browser                                                    Norca HTTP Server (/v1/event/stream)
     |--- GET /v1/event/stream?topic=Job&topic=Backup ------->| Opens chunked HTTP stream
     |        &index=1056&exact=true&region=eu-west           |
     |<-- {"Index":1056,"Events":[...]} \n -------------------| NDJSON batches + "{}\n" heartbeats
     |<-- 410 Gone (if server ring buffer evicted index 1056) |
     |    1. Triggers subscriber onResync() callbacks         |
     |    2. Subscribers refetch state -> return min(Index)   |
     |--- GET /v1/event/stream?...&index=<minIndex+1> ------->| Resumes without missing events

3. CHUNKED FRAME STREAMING (utils/classes/stream-logger.js + utils/stream-frames.js)
   Browser                                                    Norca Client/Server Agent
     |--- GET /v1/client/fs/logs/<alloc>?follow=true -------->| Streams JSON frames {"Offset":N,"Data":"..."}
     |    ReadableStream.getReader() + TextDecoder            |
     |    Decodes base64 chunks + renders ANSI via Anser      |

4. WEBSOCKET PTY STREAMING (services/sockets.js + components/exec-terminal.js)
   Browser (xterm.js)                                         Norca Client Agent
     |<== WSS /v1/client/allocation/<id>/exec?tty=true ======>| Bidirectional stdin/stdout/resize frames
```

### 4.1 Long-Polling Blocking Queries (`Watchable` Adapter)
Standard Ember Data models (`job`, `node`, `allocation`, `deployment`, `evaluation`, `volume`, `plugin`, `namespace`, `node-pool`) inherit from the `Watchable` adapter (`norca/ui/app/adapters/watchable.js:15`):
- **Index Tracking**: `WatchListService` (`norca/ui/app/services/watch-list.js:13`) maintains an in-memory map of canonical request URLs to their highest observed `X-Nomad-Index` response header (initializing at `1`).
- **Blocking Request Lifecycle**: When a route or task requests a record or collection with `{ adapterOptions: { watch: true, abortController } }`, `Watchable` appends `?index=<lastIndex>` and attaches the `AbortController.signal`.
- **Stale Record Garbage Collection (`watchable.js:130-145`)**: Upon receiving a `query()` response, `Watchable` extracts all IDs present in the JSON payload (`idsInPayload(payload)`), inspects existing records in the Ember Data store matching the query parameters, and unloads any record that was deleted server-side.

### 4.2 Multiplexed Event Stream Engine (`EventStreamService`)
To avoid opening dozens of concurrent HTTP connections on rich operational views, `EventStreamService` (`norca/ui/app/services/event-stream.js:36`) multiplexes all component subscriptions onto **at most one `/v1/event/stream` connection per `(region, namespace)` pair**:
- **Topic Union & Microtask Reconciliation (`event-stream.js:83-145`)**: As Glimmer modifiers mount and unmount during a render pass, `scheduleReconcile()` coalesces all subscription changes in a microtask (`Promise.resolve().then(...)`), computes the sorted union of requested topics (`topicsFor(key)`), and opens or updates a single `EventStreamConnection` (`norca/ui/app/utils/event-stream-connection.js:65`).
- **Gapless Resumption (`event-stream-connection.js:119-128`)**: Every delivered batch updates `this.lastIndex = message.Index`. If a connection drops (e.g., network blip or Raft leader election), it reconnects with `?index=${this.lastIndex + 1}&exact=true`, ensuring no state transition is silently skipped.
- **Exponential Backoff**: Connection retries back off exponentially from `1000ms` to `30000ms` (`event-stream-connection.js:217-223`):
  $$\text{backoff}(a) = \min\left(30000,\; 1000 \times 2^{\min(a - 1,\, 5)}\right)\;\text{ms}$$
- **`410 Gone` Resynchronization Protocol (`event-stream-connection.js:149-164`, `event-stream.js:165-177`)**: If the server's in-memory event ring buffer has rotated past the requested `index`, the server responds with `HTTP 410 Gone`. `EventStreamService.resync(key)` invokes the `onResync()` callback of every active subscriber on that connection in parallel, collects the `X-Nomad-Index` headers returned by their full state refetches, and resumes the stream from the **minimum** refetched index:
  $$\text{resumeIndex} = \min_{s \in \text{subscribers}(k)} \left(\text{Index}(s.\text{onResync}())\right)$$
  This guarantees that even the slowest-refetching component on the page misses zero events between its snapshot load and stream resumption.
- **Automatic Polling Fallback**: Views such as `Dashboard`, `BackupsList`, `PackagesList`, `QuotasList`, `ScalingList`, and `ServicesList` monitor `eventStream.stateFor(region, namespace)`. Whenever the stream state is not `'open'` (e.g., restricted ACL token or proxy buffering), they automatically fall back to a `30000ms` timer poll.

### 4.3 Token, OIDC SSO, and Client-Side ACL Evaluation
Authentication and authorization are managed by `TokenService` (`norca/ui/app/services/token.js:19`) and `utils/norca-acl.js` (`norca/ui/app/utils/norca-acl.js:1`):
- **Token Storage & Injection**: The ACL token secret is stored in `window.localStorage.nomadTokenSecret` (`token.js:31`) and injected into every API and streaming request via the `X-Nomad-Token` header (`token.js:123`).
- **One-Time Token (OTT) & OIDC Flows**: Supports exchanging one-time tokens (`POST /v1/acl/token/onetime/exchange`) and OIDC SSO login flows (`/v1/acl/oidc/auth-url`, `/v1/acl/oidc/complete-auth`).
- **Proactive TTL Expiration Warning (`token.js:153-205`)**: A background `ember-concurrency` task (`monitorTokenTime`) checks `selfToken.expirationTime` every second. When remaining TTL drops below 10 minutes (`MINUTES_LEFT_AT_WARNING = 10`), it raises a sticky warning notification with a live countdown and a one-click `Re-authenticate` action.
- **Granular Client-Side ACL Evaluator (`utils/norca-acl.js:31-143`)**: Mirrors the Go server's ACL resolution logic over the token's parsed `rulesJSON`:
  - `allowsBlock(token, block, level)`: Evaluates top-level operator blocks (`Backup`, `Quota`, `Admission`, `Operator`, `Agent`) where `deny` overrides `write` (2) and `read` (1).
  - `allowsNamespace(token, namespace, capabilities)`: Matches exact namespace rules or the longest matching glob pattern (`globMatches`) and checks namespace capabilities (`submit-job`, `write-intention`, etc.).
  - `allowsSecret(token, namespace, path, capability)`: Mirrors `acl.AllowSecretOperation` for `list-secrets`, `read-secret` (which implies `list-secrets`), `write-secret`, and `destroy-secret` across namespace capabilities and path-specific `Secrets` rules.
  - `allowsObject(token, namespace, kind, capability)`: Evaluates CRD `Objects` rules (`group/Kind`) supporting glob patterns and policy shorthands (`write` implies `read` + `write`).

---

## 5. Feature Coverage by Functional Domain

The router (`norca/ui/app/router.js:14`) and sidebar navigation (`norca/ui/app/utils/navigation.js:12`) organize the UI into seven operational domains.

| Navigation Group | UI Route (`norca/ui/app/router.js`) | Primary Component / Controller | Keyboard Shortcut | Backend REST & Streaming Endpoints |
| :--- | :--- | :--- | :--- | :--- |
| **Overview** | `/` (`index`) | `<Dashboard>` (`components/dashboard.js`) | `g` `d` | `/v1/agent/members`, `/v1/status/leader`, `/v1/operator/seal`, `/v1/jobs`, `/v1/nodes`, `/v1/deployments`, `/v1/event/stream` |
| **Workloads** | `/jobs`, `/jobs/run`, `/jobs/:job_name/*` | `jobs/*` routes & controllers | `g` `j` | `/v1/jobs`, `/v1/jobs/parse`, `/v1/job/:id/*` |
| **Workloads** | `/allocations/:allocation_id/*`, `/exec/:job_name/*` | `allocations/*`, `<ExecTerminal>` | — | `/v1/allocation/:id`, `/v1/client/fs/*`, WSS `/v1/client/allocation/:id/exec` |
| **Workloads** | `/packages`, `/packages/:namespace/:name` | `<Packages::List>`, `<Packages::Release>` | — | `/v1/releases`, `/v1/release/:ns/:name/{history,events,rollback,test,drift}` |
| **Workloads** | `/objects`, `/objects/:group/:kind/:name` | `<Objects::List>`, `<Objects::Object>` | — | `/v1/object-kinds`, `/v1/objects/:group/:kind`, `/v1/object/:group/:kind/:name` |
| **Workloads** | `/deployments`, `/deployments/:deployment_id` | `<Deployments::List>`, `<Deployments::Detail>` | — | `/v1/deployments`, `/v1/deployment/:id/*` |
| **Services & Net** | `/services`, `/services/:name` | `<Services::List>`, `<Services::Detail>` | — | `/v1/services`, `/v1/service/:name`, `/v1/mtls/roots`, `/v1/service-intentions` |
| **Services & Net** | `/network`, `/network/policies/:ns/:name`, `/network/admin/:name` | `<Network::Page>`, `<Network::Intentions>`, `<Network::Policies>` | — | `/v1/service-intentions`, `/v1/network-policies`, `/v1/admin-network-policies`, `/v1/*-policy/:name/status` |
| **Capacity** | `/clients`, `/clients/:node_id`, `/clients/:node_id/monitor` | `clients/*`, `<AgentMonitor>` | `g` `c` | `/v1/nodes`, `/v1/node/:id`, `/v1/node/pools`, `/v1/client/stats`, `/v1/agent/monitor` |
| **Capacity** | `/topology` | `TopologyController`, `<TopoViz>` | `g` `t` | `/v1/nodes?resources=true`, `/v1/allocations`, `/v1/node/pools` |
| **Capacity** | `/csi/volumes/*`, `/csi/plugins/*` | `csi/*` | `g` `r` | `/v1/volumes`, `/v1/volume/csi/:id`, `/v1/plugins`, `/v1/plugin/csi/:id` |
| **Capacity** | `/quotas`, `/quotas/new`, `/quotas/:name`, `/quotas/:name/edit` | `<Quotas::List>`, `<Quotas::Detail>`, `<Quotas::Form>` | `g` `q` | `/v1/quotas`, `/v1/quota-usages`, `/v1/quota/:name`, `/v1/quota/usage/:name`, `/v1/evaluations` |
| **Capacity** | `/scaling`, `/scaling/:policy_id` | `<Scaling::List>`, `<Scaling::Policy>` | — | `/v1/scaling/policies`, `/v1/scaling/policy/:id`, `/v1/job/:id/scale` |
| **Configuration** | `/variables`, `/variables/var/*id`, `/variables/path/*` | `variables/*` | `g` `v` | `/v1/vars`, `/v1/var/:path` |
| **Configuration** | `/secrets`, `/secrets/s/*path` | `<Secrets::List>`, `<Secrets::Detail>`, `<Secrets::SealStatus>` | — | `/v1/secrets`, `/v1/secret/:path`, `/v1/operator/seal` |
| **Configuration** | `/admission`, `/admission/:name` | `<Admission::List>`, `<Admission::Policy>` | — | `/v1/admission-policies`, `/v1/admission-policy/:name`, `/v1/agent/self` |
| **Configuration** | `/kinds`, `/kinds/:group/:kind` | `<Kinds::List>`, `<Kinds::Detail>` | — | `/v1/object-kinds`, `/v1/object-kind/:group/:kind` |
| **Operations** | `/servers`, `/servers/:agent_id`, `/servers/:agent_id/monitor` | `servers/*`, `<Servers::Regions>` | `g` `s` | `/v1/agent/members`, `/v1/operator/autopilot/health`, `/v1/status/leader`, `/v1/agent/monitor` |
| **Operations** | `/evaluations` | `EvaluationsController` | `g` `e` | `/v1/evaluations` |
| **Operations** | `/backups`, `/backups/job-history`, `/backups/:backup_id`, `/backups/:backup_id/restore`, `/backups/restores/:restore_id` | `<Backups::List>`, `<Backups::Detail>`, `<Backups::RestoreWizard>`, `<Backups::Request>` | `g` `b` | `/v1/operator/backups`, `/v1/operator/backup/:id/{contents,diff,restore,download}`, `/v1/operator/backup/restores/:id/{approve,reject}` |
| **Access** | `/access-control/{policies,roles,tokens,auth-methods}`, `/settings/tokens` | `access-control/*`, `settings/tokens` | `g` `a` / `g` `p` | `/v1/acl/policies`, `/v1/acl/roles`, `/v1/acl/tokens`, `/v1/acl/auth-methods` |

---

### 5.1 Overview, Workloads & Cluster Topology
- **Federated Overview Dashboard (`norca/ui/app/components/dashboard.js:33`)**:
  - Renders a multi-column **Federation Strip** (`--norca-columns`) showing per-region health (leader address, alive/total servers, client status, uBixVault seal status, and stacked allocation bars).
  - Aggregates global allocation counts into a lazy-loaded **Apache ECharts** doughnut chart (`Ui::Chart`, SVG renderer) alongside actionable cluster warnings (`buildWarnings`), active deployment progress bars, and a live ticker of the 8 most recent cluster events across all scoped regions.
- **Jobs, Templates, Allocations, Logs & WebSocket Exec (`norca/ui/app/router.js:15-88`)**:
  - Full lifecycle management for `service`, `batch`, `system`, and `sysbatch` jobs across 14 sub-routes (`task-group`, `definition`, `scaling`, `placement`, `events`, `versions`, `deployments`, `dispatch`, `evaluations`, `allocations`, `clients`, `services`, `variables`).
  - Interactive job submission (`/jobs/run`) with reusable job templates (`/jobs/run/templates`).
  - Allocation filesystem browser (`/allocations/:id/fs/*path`), ANSI-rendered live log streaming (`StreamLogger`), and browser-based interactive PTY shell (`/exec/:job/:group/:task`) powered by `xterm.js` over WebSocket (`/v1/client/allocation/:id/exec?tty=true&ws_handshake=true`).
- **Clients, Servers, Node Pools & Interactive Topology (`norca/ui/app/templates/topology.hbs:8`)**:
  - `<Clients>` and `<Topology>` filter nodes by State, Node Pool (`/v1/node/pools`), Class, Datacenter, Version, and Host Volume, with persistent saved views (`Ui::SavedViews`).
  - `<TopoViz>` renders a D3 canvas packing client nodes by datacenter with live CPU/Memory utilization bars and interactive allocation placement links, refreshed automatically on `Node` or `Allocation` event stream updates (`<Topology::LiveUpdates>`).
  - `<Servers>` displays Raft autopilot health, voter/non-voter status, federation peers, and live agent log monitors (`/v1/agent/monitor`).

### 5.2 Packaging & Releases (`/ui/packages`)
Implemented in `<Packages::List>` (`norca/ui/app/components/packages/list.js:30`) and `<Packages::Release>` (`norca/ui/app/components/packages/release.js:37`), backed by `norca/ui/app/utils/releases.js`:
- **Multi-Region Release Catalog (`/ui/packages`)**: Lists all installed pack releases across scoped regions and namespaces, displaying Pack Name/Version, active Revision number, lifecycle Phase (`deployed`, `pending`, `failed`, `superseded`, `uninstalling`), real-time Drift indicator (`drifted` vs `in sync`), target regions, and cryptographic signature verification badge (`signature(rel)`). Subscribes to `CustomObject:packs.norca.io/Release` events.
- **Release Inspector & Operations (`/ui/packages/:namespace/:name`)**:
  - Displays applied resources in deterministic apply order (`Status.Resources`), Kubernetes-style status conditions (`Status.Conditions`), lifecycle hook execution results (`Status.Hooks`), last test suite status (`Status.LastTest`), and multi-region synchronization matrix (`regionStates`).
  - **Live Drift Detection (`runDrift`, `release.js:321`)**: Triggers `POST /v1/release/:ns/:name/drift` to compare live cluster objects against the release's desired state and highlights drifted fields.
  - **Revision Diffing & Dry-Run Rollback (`release.js:185-430`)**: Compares any two historical revisions (`compareRevisions(a, b)`) and provides a two-step rollback modal that first executes a server-side dry-run (`DryRun: true`) to preview exact resource changes before committing the rollback revision.
  - **Test Execution & Uninstall (`release.js:340-470`)**: Runs pack test jobs (`POST /v1/release/:ns/:name/test`) and supports uninstalling releases with an optional `skipHooks` toggle.

### 5.3 Security, Secrets, mTLS & Network Policies
- **Built-In Secret Store (`/ui/secrets`, `/ui/secrets/s/*path`)**:
  - `<Secrets::List>` (`norca/ui/app/components/secrets/list.js:33`) lists secret paths, current version, state (`active`, `deleted`, `destroyed`), owner, expiration status, and real-time audit correlation from the `SecretAccess` event stream topic (`latestAccesses`). Secret values are **never** fetched or rendered in list views.
  - `<Secrets::Detail>` (`norca/ui/app/components/secrets/detail.js:38`) displays secret metadata, SHA-256 value hashes (`shortHash`), rotation intervals (`RotateEvery`), and scans up to `JOB_SCAN_LIMIT` jobs in the namespace to list workloads referencing the secret (`findReferencingJobs`).
  - **Explicit Value Reveal & CAS Writes (`detail.js:247-411`)**: Secret values are fetched only when the operator explicitly clicks reveal (`<Secrets::Reveal>`), and all version operations (`delete`, `undelete`, `destroy` with typed confirmation, and `restore` as a new version) enforce Check-And-Set (`cas: s.ModifyIndex`) concurrency control, handling `409 Conflict` gracefully.
- **Region Seal Status (`<Secrets::SealStatus>`, `norca/ui/app/components/secrets/seal-status.js:22`)**:
  - Queries `GET /v1/operator/seal` (for tokens with `operator:read`) and renders per-server keyring seal state (`unsealed`, `sealed`, `not initialized`), Shamir threshold progress (`Progress of Threshold shares entered`), and uBixVault secret store core status (`active`, `standby`, `sealed`, `needs recovery`). By security design, unseal shares are never entered via the browser.
- **Service Catalog, mTLS CA Status & Service Intentions (`/ui/services`, `/ui/network`)**:
  - `<Services::List>` (`norca/ui/app/components/services/list.js:29`) queries `/v1/services`, `/v1/service/:name`, and `/v1/mtls/roots`, displaying active mTLS Trust Domain and root CA certificate validity (`NotAfter`) alongside per-region instance health (`passing`, `warning`, `critical`, and instances excluded from discovery due to readiness checks).
  - `<Services::Detail>` (`norca/ui/app/components/services/detail.js:27`) breaks down instance health checks by role (`startup`, `readiness`, `liveness`), discovery eligibility (`inDiscovery`), and incoming/outgoing mTLS service intentions.
  - `<Network::Intentions>` (`norca/ui/app/components/network/intentions.js:35`) visualizes mTLS service-to-service authorization (`GET/PUT/DELETE /v1/service-intention`) both as a filterable table and an interactive **ECharts circular directed graph** (`type: 'graph', layout: 'circular'`) with high-contrast solid green (`#2f9e44`) edges for `allow` and dashed red (`#e03131`) edges for `deny`.
  - `<Network::Policies>` (`norca/ui/app/components/network/policies.js:22`) and `<Network::Policy>` inspect namespace L3/L4 network policies (`/v1/network-policies`) and cluster-wide admin network policies (`/v1/admin-network-policies`), including ingress/egress CIDR, namespace, and workload selector rules plus per-client enforcement status (`/status`).
- **Access Control (`/ui/access-control/*`)**: Full CRUD management for ACL Policies, Roles, Tokens, and OIDC/JWT Auth Methods.

### 5.4 Governance & Extensibility (`/ui/admission`, `/ui/quotas`, `/ui/objects`, `/ui/kinds`)
- **Admission Control Policies (`/ui/admission`, `/ui/admission/:name`)**:
  - `<Admission::List>` (`norca/ui/app/components/admission/list.js:22`) and `<Admission::Policy>` (`norca/ui/app/components/admission/policy.js:21`) manage CEL admission policies (`/v1/admission-policies`) that `deny` or `warn` on Job, Variable, and Release writes, including `crd-schema` validation and trusted signature keys (`TrustedKeys`).
  - Inspects `/v1/agent/self` to display configured server admission webhooks (`config.Server.Admission.Webhooks`) and subscribes to the `AdmissionPolicy` event stream topic to render a live feed of `JobAdmissionDenied` and `VariableAdmissionDenied` rejections as they occur.
- **Multi-Dimensional Namespace Quotas (`/ui/quotas`, `/ui/quotas/:name`)**:
  - `<Quotas::List>` (`norca/ui/app/components/quotas/list.js:30`) and `<Quotas::Detail>` (`norca/ui/app/components/quotas/detail.js:26`) evaluate `QuotaSpec` limits against regional `QuotaUsage` across 5 dimensions (`norca/ui/app/utils/quotas.js:13`): **CPU (`MHz`)**, **Memory (`MB`)**, **Memory Max (`MB`)**, **Allocations (count)**, and **Variables (`MiB`)**.
  - Renders stacked utilization bars colored dynamically by saturation ratio $r = \frac{\text{used}}{\text{limit}}$: green ($r < 0.75$), orange ($0.75 \le r < 0.90$), and red ($r \ge 0.90$, `NEARLY_FULL`).
  - Automatically queries `/v1/evaluations?filter=Status == "blocked" and QuotaLimitReached != ""` and subscribes to `Quota` + `Evaluation` topics to display jobs currently blocked by exhausted quota dimensions (`quotaBlocked`).
- **Custom Objects & Type Definitions (`/ui/objects`, `/ui/kinds`)**:
  - `<Kinds::List>` (`norca/ui/app/components/kinds/list.js:36`) lists registered CRD schemas (`/v1/object-kinds`), correlating each kind with the Pack Release that installed it (`kindOwners`), its scope (`namespace` vs `cluster`), CEL validation rules, scale subresource paths (`Scale.SpecPath`), and reconciler webhooks.
  - `<Objects::List>` (`norca/ui/app/components/objects/list.js:29`) dynamically constructs table columns from the kind's JSONPath definitions (`evaluatePath(o, c.Path)`), displays desired vs. current scale, evaluates condition health (`objectHealth`), checks `Generation` vs. `Status.observedGeneration`, and subscribes to `CustomObject:<group>/<Kind>` events for live updates.

### 5.5 Operations, Autoscaling & Disaster Recovery (`/ui/backups`, `/ui/scaling`)
- **Automated Encrypted Backups & Two-Person Restore (`/ui/backups/*`)**:
  - `<Backups::List>` (`norca/ui/app/components/backups/list.js:34`) displays regional backup schedules (`Enabled`, `NextDue`, `Overdue`, `LastSuccessID`, `LastError`), pending full-restore approval requests, and the historical backup table with multi-destination verification status, sizes, labels, pin toggles (`updateBackup`), and manual trigger modal (`<Backups::BackupNow>`).
  - `<Backups::Detail>`, `<Backups::Contents>`, and `<Backups::Compare>` inspect unencrypted manifest summaries, prompt ephemerally for the operator's `age` private identity (`<Backups::Identity>`) to decrypt and browse archived jobs/variables/ACL objects in memory, or diff a backup against the live cluster (`/v1/operator/backup/:id/diff`).
  - `<Backups::RestoreWizard>` (`norca/ui/app/components/backups/restore-wizard.js:37`) guides operators through a 5-step **Selective Restore** (`Mode` $\rightarrow$ `Items` $\rightarrow$ mandatory server `Preview` (`DryRun: true`) $\rightarrow$ `Confirm` with per-variable CAS overwrite toggles $\rightarrow$ `Result`) or a 4-step **Full Region Restore** (`Mode` $\rightarrow$ `Preview` $\rightarrow$ typed region name `Confirm` $\rightarrow$ `Approval`).
  - `<Backups::Request>` (`norca/ui/app/components/backups/request.js:33`) enforces the **Two-Person Rule** in the UI: it compares `this.token.selfToken.id` against `restore.RequestedBy` (`isRequester`, line 69) to prevent self-approval, displays the full blast-radius preview to the second management administrator, collects their `age` identity to approve (`POST /v1/operator/backup/restores/:id/approve`) or reject with a reason, and streams live execution progress (`approved` $\rightarrow$ `running` $\rightarrow$ `complete`) along with the automatic pre-restore safety backup ID (`PreRestoreBackupID`).
- **Built-In & External Autoscaling (`/ui/scaling`, `/ui/scaling/:policy_id`)**:
  - `<Scaling::List>` (`norca/ui/app/components/scaling/list.js:31`) and `<Scaling::Policy>` (`norca/ui/app/components/scaling/policy.js:27`) distinguish policies evaluated by Norca's **built-in leader autoscaler** (`?engine=builtin`) from external autoscalers, displaying bounds (`Min`–`Max`), cooldowns, check error behavior (`OnCheckError`), deployment scaling policy (`ScaleDuringDeployment`), and an **ECharts step-line time-series chart** (`step: 'end'`) with dashed `min`/`max` markLines tracking group count changes over time alongside live `Scaling` event stream decisions.

---

## 6. Dual-UI Asset Embedding & Cross-Platform Build Pipeline

### 6.1 Dual-UI Coexistence Strategy (`/ui/` vs `/ui/legacy/`)
During the transition period, the previous Nomad v1.6.5 UI remains accessible at `/ui/legacy/` (linked directly from the topbar profile menu and the `Ctrl+K` command palette in `norca/ui/app/components/frame/topbar.hbs:74` and `command-palette.js:90`).

Both single-page applications are compiled into a single directory tree (`norca/ui/dist/`) before being packed into Go source code (`norca/command/agent/bindata_assetfs.go`) under the `ui` build tag:
- `ui/dist/index.html` and `ui/dist/assets/*` $\rightarrow$ New Norca UI (configured with `rootURL: '/ui/'`).
- `ui/dist/legacy/index.html` and `ui/dist/legacy/assets/*` $\rightarrow$ Legacy UI (configured with `rootURL: '/ui/legacy/'`).

### 6.2 Server-Side Virtual Filesystem Routing (`UIAssetWrapper`)
In `norca/command/agent/http.go:538-552`, when both the compile-time `uiEnabled` flag (`-tags ui`) and runtime `agentConfig.UI.Enabled` setting are true, the HTTP server mounts the embedded asset filesystem at `/ui/`:

```go
s.mux.Handle("/ui/", http.StripPrefix("/ui/", s.handleUI(
    agentConfig.UI.ContentSecurityPolicy,
    http.FileServer(&UIAssetWrapper{FileSystem: assetFS()}),
)))
```

Because both Ember applications use `locationType: 'history'`, browser navigation to deep client-side URLs (such as `/ui/packages/default/webapp` or `/ui/legacy/jobs/webapp`) requests paths that do not exist as physical files in `assetFS()`. `UIAssetWrapper.Open(name)` (`norca/command/agent/http.go:644-674`) intercepts `os.ErrNotExist` and dispatches to the appropriate SPA entry point:

1. If `fs.FileSystem.Open(name)` succeeds (e.g., `/ui/assets/nomad-ui.js` or `/ui/legacy/assets/vendor.js`), the exact static asset is served with its fingerprinted filename.
2. If `err == os.ErrNotExist` and `isLegacyUIPath(name)` is true (`name == "legacy"` or `strings.HasPrefix(name, "legacy/")`), `UIAssetWrapper` opens and serves `"legacy/index.html"` (`legacyUIIndex`), allowing the legacy Ember router to boot and handle the route—or falls back to `"index.html"` if the binary was built with `nolegacy`.
3. For all other `os.ErrNotExist` paths under `/ui/*`, `UIAssetWrapper` opens and serves `"index.html"`, booting the new Norca UI router.
4. Requests to the root path `/` are handled by `handleRootFallthrough()` (`http.go:701-713`), which issues an `HTTP 307 Temporary Redirect` to `/ui/` (preserving any raw query string parameters such as `?region=`).
5. If the binary is built without `-tags ui`, `norca/command/agent/stub_asset.go:13` sets `uiEnabled = false` and serves a static HTML notice explaining how to build with `make release` or `make dev-ui`.

### 6.3 Linux vs. Windows Hybrid Build Pipeline

```
+---------------------------------------------------------------------------------------------------+
|                           DUAL-UI BUILD & EMBEDDING PIPELINE COMPARISON                           |
+---------------------------------------------------------------------------------------------------+

  LINUX (norca/GNUmakefile: prerelease)              WINDOWS (windows.bat)
  =====================================              =====================
  1. make ember-dist (GNUmakefile:390)               1. Extract Legacy UI (windows.bat:60)
     - cd ui && yarn install                            - Legacy node-sass/tooling fails on Windows
     - npm run build -> writes ui/dist/*                - Runs: go run -tags ui,codegen_generated
                                                          ./scripts/legacyui build\ui-legacy-dist
                                                        - Extracts `dist/legacy/*` bytes from the
                                                          existing command/agent/bindata_assetfs.go
                   |                                                  |
                   v                                                  v
  2. make ember-dist-legacy (GNUmakefile:405)        2. Build New UI (windows.bat:65)
     - git archive LEGACY_UI_REF (8a1a0fc61)            - pushd norca\ui && yarn install --frozen-lockfile
       into build/ui-legacy                             - npm run build (empties & writes ui\dist\*)
     - Verifies ui-legacy/{package.json,yarn.lock}                    |
       match LEGACY_UI_REF for SBOM/license notices                   v
     - Rewrites rootURL: '/ui/' -> '/ui/legacy/'     3. Restore Legacy Tree (windows.bat:73)
     - yarn install && npm run build                    - xcopy build\ui-legacy-dist -> ui\dist\legacy
     - Copies build/ui-legacy/dist -> ui/dist/legacy
                   |                                                  |
                   +------------------------+-------------------------+
                                            |
                                            v
                   4. Generate Embedded VFS (GNUmakefile:376 / windows.bat:77)
                      go-bindata-assetfs -pkg agent -prefix ui -modtime 1480000000 \
                        -tags ui -o command/agent/bindata_assetfs.go ./ui/dist/...
                                            |
                                            v
                   5. Compile Single Binary & SBOM
                      go build -trimpath -tags ui,codegen_generated,release ...
```

- **Why `norca/ui-legacy/` and `norca/scripts/legacyui/main.go` Exist**:
  - **License & SBOM Integrity (`norca/ui-legacy/`)**: Even though the legacy UI source is archived at commit `8a1a0fc61` (`LEGACY_UI_REF` in `norca/GNUmakefile:402`), its exact `package.json` and `yarn.lock` are kept at `norca/ui-legacy/` so `tools/thirdparty` (`make notices` and `make check-notices`) can audit and include every third-party JavaScript dependency of both UIs in `THIRD_PARTY_NOTICES.md`. Step `ember-dist-legacy` (`GNUmakefile:410`) runs `cmp` to enforce that `ui-legacy/package.json` and `ui-legacy/yarn.lock` match `LEGACY_UI_REF` byte-for-byte.
  - **Windows Build Compatibility (`norca/scripts/legacyui/main.go:1`)**: The legacy Ember 3.28 UI depends on older Node/Sass tooling that fails to compile from source on native Windows environments, whereas the new Ember 6.12 UI in `norca/ui/` builds natively on Windows with Node $\ge 20$. Because `ember build` wipes `norca/ui/dist/` before writing the new UI, `windows.bat:60-74` first invokes `go run -tags ui,codegen_generated ./scripts/legacyui` to extract all assets prefixed with `"dist/legacy/"` from the committed `command/agent/bindata_assetfs.go` into `norca/build/ui-legacy-dist`, builds the new UI from source, copies the extracted legacy tree back into `norca/ui/dist/legacy`, and regenerates `command/agent/bindata_assetfs.go` with deterministic modification timestamps (`-modtime 1480000000`).

---

## 7. Server HTTP Configuration & Security Headers Reference

Operators configure the Web UI, Content Security Policy, environment labels, and external Consul/Vault deep links in `norca.hcl` via the top-level `ui` stanza (`norca/norca/structs/config/ui.go:16`):

```hcl
ui {
  enabled = true

  # Optional environment badge displayed in the top navigation bar and page title
  label {
    text             = "production-eu"
    background_color = "#d63939"
    text_color       = "#ffffff"
  }

  # Strict Content-Security-Policy header injected by HTTPServer.handleUI (http.go:693)
  content_security_policy {
    default_src     = ["'none'"]
    connect_src     = ["*"]
    img_src         = ["'self'", "data:"]
    script_src      = ["'self'"]
    style_src       = ["'self'", "'unsafe-inline'"]
    form_action     = ["'none'"]
    frame_ancestors = ["'none'"]
  }

  # Optional deep-link integrations in the topbar menu and task/service views
  consul {
    ui_url = "https://consul.internal.example.com:8500/ui"
  }

  vault {
    ui_url = "https://vault.internal.example.com:8200/ui"
  }
}

# Custom HTTP API response headers applied to all /v1/* endpoints (http.go:749)
http_api_response_headers {
  "X-Frame-Options"           = "DENY"
  "X-Content-Type-Options"    = "nosniff"
  "Strict-Transport-Security" = "max-age=31536000; includeSubDomains"
}
```

### Security & HTTP Protection Mechanisms in `command/agent/http.go`:
1. **Strict Default CSP (`norca/structs/config/ui.go:103-113`)**: By default, `DefaultCSPConfig()` emits:
   ```http
   Content-Security-Policy: default-src 'none'; connect-src *; img-src 'self' data:; script-src 'self'; style-src 'self' 'unsafe-inline'; form-action 'none'; frame-ancestors 'none'
   ```
   This prevents clickjacking (`frame-ancestors 'none'`), blocks inline script injection (`script-src 'self'`), and forbids form exfiltration (`form-action 'none'`).
2. **Untrusted Workload Content Sanitization (`http.go:1140-1155`)**: Endpoints that stream or return user-controlled task files (`/v1/client/fs/cat`, `/v1/client/fs/readat`) are wrapped in `s.wrapUntrustedContent()`, which uses `helper/noxssrw.NewResponseWriter` to force safe `text/plain` MIME types and prevent browsers from rendering workload artifacts as HTML/JavaScript.
3. **Per-IP Connection Limiting & TLS Handshake Deadlines (`http.go:246-324`)**: `makeConnState()` enforces `https_handshake_timeout` on `http.StateNew` connections to mitigate slowloris attacks and applies a token-bucket rate-limited `429 Too Many Requests` responder (`connLimiter`) when a client exceeds `http_max_conns_per_client`.

---

## 8. CLI & Developer Command Cheat-Sheet

| Command | Platform | Action & Description |
| :--- | :--- | :--- |
| `cd norca/ui && yarn start` | Linux / macOS / Windows | Starts the local Ember development server (`http://localhost:4200/ui/`) with Mirage mock cluster or proxy. |
| `cd norca/ui && yarn test` | Linux / macOS / Windows | Runs template class verification (`lint:classes`), Handlebars lint (`lint:hbs`), ESLint (`lint:js`), and QUnit suite. |
| `cd norca/ui && node scripts/check-template-classes.js` | Linux / macOS / Windows | Verifies zero Bulma/HDS/FlightIcon regressions and strict `Ui::*` component layer encapsulation across all `.hbs` files. |
| `make -C norca ember-dist` | Linux / macOS | Installs `norca/ui` dependencies and builds production assets into `norca/ui/dist/`. |
| `make -C norca ember-dist-legacy` | Linux / macOS | Archives `LEGACY_UI_REF`, verifies `ui-legacy/yarn.lock`, sets `rootURL: '/ui/legacy/'`, and builds `norca/ui/dist/legacy/`. |
| `make -C norca static-assets` | Linux / macOS | Runs `go-bindata-assetfs` over `norca/ui/dist/...` to regenerate `norca/command/agent/bindata_assetfs.go`. |
| `make -C norca dev-ui` | Linux / macOS | Builds the new UI, embeds `bindata_assetfs.go`, and compiles a development `norca` binary with `-tags ui`. |
| `windows.bat` | Windows | Extracts `/ui/legacy/` from `bindata_assetfs.go`, builds `norca/ui`, re-embeds both UIs, compiles `norca.exe` + SBOMs. |
| `windows.bat skipui` | Windows | Compiles `norca.exe` using the existing committed `bindata_assetfs.go` without requiring Node.js or Yarn. |
| `windows.bat nolegacy` | Windows | Builds and embeds only the new `norca/ui` (`/ui/legacy/` then falls back to `index.html`). |

---

## 9. Summary for Operators and Frontend Contributors

1. **Always Build Release Binaries with `-tags ui`**: Without the `ui` build tag, `command/agent/stub_asset.go` compiles a stub filesystem and `/ui/` serves a static disabled message. Both `make release` and `windows.bat` enable `-tags ui,codegen_generated,release` automatically.
2. **Never Use Raw Tabler Component Classes in Page Templates**: Page and feature templates (`app/templates/**`, `app/components/<feature>/**`) must compose `<Ui::Card>`, `<Ui::Button>`, `<Ui::Badge>`, `<Ui::DataTable>`, `<Ui::Modal>`, `<Ui::Tabs>`, and `<Ui::Alert>` rather than raw `.card`, `.btn`, or `.table` classes. Always run `yarn lint:classes` before committing template changes.
3. **Use `EventStreamService` + `apiRequest` for New Feature Domains**: New Norca operational pages should fetch typed JSON state via `apiRequest(this.token, path, { params: { region, namespace } })` (`norca/ui/app/utils/norca-api.js:62`) and subscribe to regional topics via `this.eventStream.subscribe({ topics, region, namespace, onEvents, onResync })` so all views in the same region share a single multiplexed `/v1/event/stream` connection.
4. **Preserve Ephemeral Cryptographic Boundaries**: Never persist `age` decryption identities, plaintext secret items, or one-time tokens to `window.localStorage` or route query parameters. Keep decryption identities strictly inside `BackupsService` (`norca/ui/app/services/backups.js:21`) and gate all mutating UI controls with `utils/norca-acl.js` helpers (`allowsBlock`, `allowsNamespace`, `allowsSecret`, `allowsObject`).
