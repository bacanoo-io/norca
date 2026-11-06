<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Custom Objects and Operator Framework Architecture

## 1. Executive Summary & Core Architectural Principles

Historically, HashiCorp Nomad provided a fast, single-binary workload scheduler for batch and service jobs, but lacked a declarative extension model comparable to Kubernetes Custom Resource Definitions (CRDs) and the Operator pattern. Platform engineers attempting to build higher-level abstractions on Nomad (such as managed database clusters, ingress load balancers, or declarative package releases) had to rely on external state stores, bespoke polling daemons, or unstructured job metadata without server-side schema validation, optimistic concurrency separation between desired (`spec`) and observed (`status`) state, pre-deletion hooks (`finalizers`), or cascading garbage collection.

Norca bridges this gap by introducing a **native, zero-etcd Custom Object and Operator Framework** built directly into the Raft consensus state machine (`norca/norca/object_endpoint.go`, `norca/norca/objectschema/`, `norca/norca/objectgc/`) paired with a first-party **Go Controller SDK** (`norca/controller/`).

### Key Architectural Tenets:
1. **Declarative Extensibility Without etcd Bloat**: Custom object types (`ObjectKind`) and instances (`CustomObject`) live natively inside Norca's in-memory MemDB tables (`object_kinds`, `custom_objects`, `object_events`, `object_tombstones`) backed by the existing region Raft log. No external database or API aggregation layer is required.
2. **Self-Contained Draft 2020-12 JSON Schema & CEL Validation**: Every `ObjectKind` compiles a self-contained Draft 2020-12 JSON Schema (`github.com/santhosh-tekuri/jsonschema/v6`) with recursive structural default injection (`ApplyDefaults`) and Google CEL (Common Expression Language) transition rules comparing `self` and `oldSelf` (including unit-aware `quantity()` arithmetic).
3. **Strict `spec` vs. `status` Separation & Optimistic Concurrency**: Users modify desired state (`spec`) via `CustomObject.Apply` (requiring the `write` ACL capability and incrementing `Generation`), whereas controllers report observed state (`status`, `observedGeneration`, `conditions`) via `CustomObject.UpdateStatus` (requiring the `write-status` capability without incrementing `Generation`). Every Raft mutation enforces check-and-set (`ExpectedModifyIndex`) to eliminate lost updates.
4. **Finalizers & Three-Mode Cascading Garbage Collection**: Pre-deletion hooks (`Finalizers`) transition deleted objects into a `Deleting` state (`DeletionTime > 0`) until external controllers clean up provisioned resources and remove their finalizer string. A leader-elected garbage collector (`objectgc.Collector`) sweeps dependent custom objects and stops owned Nomad jobs (`norca.owner`) across `background`, `foreground`, and `orphan` propagation modes using durable `ObjectTombstone` records.
5. **In-Place CEL Schema Migrations & Per-Kind Load Management**: Raising an `ObjectKind`'s `version` triggers both synchronous dry-run migration verification across all stored objects and throttled background leader migration (`objectMigrationLoop`) alongside lazy migrate-on-write, while per-kind token-bucket rate limiters (`WriteRate`, `IdentityWriteRate`) and watch caps (`MaxWatchesPerIdentity`) protect the Raft leader from runaway operators.
6. **Level-Triggered Go Operator SDK (`norca/controller/`)**: A lightweight, dependency-minimal Go runtime providing `Informer` caches synchronized via the Norca `CustomObject` Event Stream (with automatic `410 Gone` resync and blocking-query fallback), deduplicating workqueues with exponential backoff, clock-skew-tolerant distributed `LeaderElector` leases over Variables, and type-bound HTTPS admission webhooks.

```
+---------------------------------------------------------------------------------------------------+
|                                      NORCA REGION LEADER                                          |
|                                                                                                   |
|  1. Kind Registration (norca object-kind apply)                                                   |
|  +---------------------------------------------------------------------------------------------+  |
|  | ObjectKind Endpoint (object_endpoint.go:253)                                                |  |
|  |   ├── objectschema.Compile() -> Draft 2020-12 JSON Schema + Default Validator (schema.go:56)|  |
|  |   ├── ObjectRuleEngine.CompileRules() -> CEL self / oldSelf / quantity() (object.go:172)    |  |
|  |   ├── MigrationEngine.Compile() -> CEL Spec/Status Version Converters (migration.go:167)    |  |
|  |   └── checkStoredObjects() -> Dry-run validates all existing objects against new schema     |  |
|  +----------------------------------------------+----------------------------------------------+  |
|                                                 | Raft Apply (ObjectKindUpsertRequestType)        |
|                                                 v                                                 |
|  2. Object Write Pipeline (norca object apply / status / scale / delete)                          |
|  +---------------------------------------------------------------------------------------------+  |
|  | CustomObject.writeObject() (object_endpoint.go:649)                                         |  |
|  |   ├── Per-Identity & Per-Kind Token Bucket Limiter (object_ecosystem.go:147)                |  |
|  |   ├── Lazy Migrate-on-Write if obj.KindVersion < kind.Version (object_ecosystem.go:337)     |  |
|  |   ├── Defaulting Webhooks (HTTPS POST -> JSON Patch /spec) + schema.ApplyDefaults()         |  |
|  |   ├── schema.Validate({"spec": ..., "status": ...}) + CEL Rules Evaluation                  |  |
|  |   └── Validating Webhooks -> Raft CAS Apply (CustomObjectUpsertRequestType)                 |  |
|  +----------------------------------------------+----------------------------------------------+  |
|                                                 |                                                 |
|                                                 v                                                 |
|  +---------------------------------------------------------------------------------------------+  |
|  | Raft FSM & MemDB State Store (state_store_objects.go / state_store_objects_lifecycle.go)    |  |
|  |   ├── Table: "object_kinds"      (Index: id="group/Kind\x00")                               |  |
|  |   ├── Table: "custom_objects"    (Indexes: id="group/Kind\x00ns\x00name\x00", ns, labels)   |  |
|  |   ├── Table: "object_events"     (Ring buffer: 20 events/object, deduplicated Count)        |  |
|  |   └── Table: "object_tombstones" (Retained 5m for cascading GC of child objects & jobs)     |  |
|  +-------------------+-----------------------------------------------------+-------------------+  |
+----------------------|-----------------------------------------------------|----------------------+
                       | Event Stream (Topic: "CustomObject")                | Leader Background Workers
                       v                                                     v
+----------------------------------------------------+   +------------------------------------------+
|         GO OPERATOR / CONTROLLER PROCESS           |   |    OBJECT GC & MIGRATION LOOPS           |
|              (norca/controller/)                   |   |  (objectgc.go:173 / ecosystem.go:815)    |
|                                                    |   |                                          |
|  +----------------------------------------------+  |   |  - Foregrounds: deletes owned objects &  |
|  | Informer Cache (informer.go:52)              |  |   |    waits for owned jobs to reach Dead    |
|  | List + Watch Stream (exact index resumption) |  |   |    before removing parent object.        |
|  +----------------------+-----------------------+  |   |  - Backgrounds: sweeps dependents from   |
|                         | Predicate(old, new)      |   |    ObjectTombstone records.              |
|                         v                          |   |  - Migrations: converts 200 objects/5s   |
|  +----------------------------------------------+  |   |    via CEL when kind.Version increases.  |
|  | Deduplicating Workqueue (queue.go:19)        |  |   +------------------------------------------+
|  | MaxOf(ExponentialBackoff[500ms..5m], 10 QPS) |  |
|  +----------------------+-----------------------+  |
|                         | LeaderElector Gate       |
|                         v                          |
|  +----------------------------------------------+  |
|  | Reconciler.Reconcile(ctx, key)               |  |
|  |   1. AddFinalizer / Handle Deleting()        |  |
|  |   2. Ensure child Jobs (meta: norca.owner)   |  |
|  |   3. status.Observe(obj) & SetCondition()    |  |
|  |   4. Objects().UpdateStatus() (CAS retry)    |  |
|  |   5. Recorder().Event() (1m dedup window)    |  |
|  +----------------------------------------------+  |
+----------------------------------------------------+
```

---

## 2. Object Kinds (`ObjectKind`) & Schema Validation

An `ObjectKind` (`norca/norca/structs/objects.go:117`) registers a custom type in the cluster's type registry (`TableObjectKinds = "object_kinds"`). Every kind is uniquely identified by its DNS subdomain `Group` (`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`, max 253 chars) and CamelCase `Kind` (`^[A-Za-z][A-Za-z0-9]{0,62}$`), forming the canonical reference `<group>/<Kind>` (e.g., `db.example.com/PostgresCluster` or `norca.io/LoadBalancer`).

### 2.1 Core `ObjectKind` Structure & Constraints
Defined in `norca/norca/structs/objects.go:38-169` and `objects_ecosystem.go:65-163`:

| Field | Type | Validation / Constraint | Purpose |
| :--- | :--- | :--- | :--- |
| **`Group`** | `string` | Lowercase DNS subdomain ($\le 253$ chars); `packs.norca.io` is reserved for built-in release kinds | Logical API group namespace. |
| **`Kind`** | `string` | CamelCase identifier ($\le 63$ chars) | Resource type name within the group. |
| **`Namespaced`** | `bool` | Immutable while objects of the kind exist (`state_store_objects.go:244`) | If `true`, objects reside in a specific namespace; if `false`, cluster-scoped (`Namespace = ""`) and authorized via `namespace "*"` ACLs. |
| **`Schema`** | `string` | Non-empty JSON object, $\le 256\text{ KiB}$ (`MaxObjectKindSchemaSize`) | Self-contained Draft 2020-12 JSON Schema describing `{"spec": ..., "status": ...}`. |
| **`Rules`** | `[]*ObjectKindRule` | Up to 64 rules; expression $\le 8192$ chars, message $\le 1024$ chars | Google CEL validation & transition rules evaluated on writes. |
| **`Columns`** | `[]*ObjectKindColumn`| Up to 16 columns; path $\le 256$ chars | Custom printer columns extracted via dotted/filter paths for `norca object list` and the Web UI. |
| **`Scale`** | `*ObjectKindScale` | `SpecPath` must start with `spec.`; `StatusPath` with `status.` | Maps the `/scale` subresource for `norca object scale` and the Nomad autoscaler. |
| **`Version`** | `uint64` | Monotonically non-decreasing ($\ge 1$, defaults to `1`) | Schema version tracked on stored objects (`CustomObject.KindVersion`). |
| **`Migration`** | `*ObjectKindMigration`| CEL expressions $\le 16\text{ KiB}$ each | CEL transformation programs (`Spec`, `Status`) converting older stored objects to `Version`. |
| **`Webhooks`** | `[]*ObjectKindWebhook`| Up to 8 HTTPS webhooks; timeout $\le 30\text{s}$ (default $5\text{s}$) | Type-bound `default` and `validate` admission webhooks. |
| **`Limits`** | `*ObjectKindLimits` | Token bucket QPS/burst, object count, and watch limits | Server-side rate limiting and quota protection per kind. |

### 2.2 JSON Schema Compilation & Structural Defaulting (`norca/norca/objectschema/`)
When an `ObjectKind` is registered via `ObjectKind.UpsertObjectKind` (`norca/norca/object_endpoint.go:253`), the server compiles its `Schema` through `objectschema.Compile(group, kind, doc)` (`norca/norca/objectschema/schema.go:56`):

1. **Draft 2020-12 & SSRF Prevention (`noLoader`)**:
   - The compiler (`github.com/santhosh-tekuri/jsonschema/v6`) defaults to `jsonschema.Draft2020` and registers the virtual resource URI `https://objectkinds.norca.invalid/<group>/<Kind>.json`.
   - To prevent Server-Side Request Forgery (SSRF) or local file disclosure, `c.UseLoader(noLoader{})` (`schema.go:48-52`) unconditionally rejects any external `$ref` URL. Schemas must be 100% self-contained, using only internal JSON Pointer references (`#/$defs/...`).
2. **Compile-Time Default Self-Validation (`checkDefaults`)**:
   - Before accepting a schema, `s.checkDefaults(c, loc)` (`schema.go:101-146`) recursively walks `properties`, `$defs`, `definitions`, `patternProperties`, `items`, `additionalProperties`, `not`, `allOf`, `anyOf`, `oneOf`, and `prefixItems` up to `maxDefaultDepth = 64`.
   - Every `"default"` value declared anywhere in the schema is compiled against its own sub-schema fragment and validated. This guarantees that a schema can never inject a default value that its own validation rules would reject.
3. **Deterministic Recursive Default Injection (`ApplyDefaults`)**:
   - On every object write (`CustomObject.Apply`, `CustomObject.UpdateStatus`, and schema migrations), `schema.ApplyDefaults(doc)` (`schema.go:229-293`) mutates the `{"spec": ..., "status": ...}` document in place.
   - It resolves local `$ref` chains (`deref`, `schema.go:298`) and `allOf` compositions, iterates object `properties` in sorted lexicographical order for determinism, injects deep-copied default values for missing keys, and recurses into nested objects, `additionalProperties`, and array `items`.
4. **ModifyIndex-Keyed Schema Caching**:
   - Compiled schemas are cached in `objectRegistry.schemas` keyed by `kind.KindRef()` and invalidated automatically whenever `kind.ModifyIndex` changes (`norca/norca/object_endpoint.go:92-116`).

### 2.3 CEL Validation & Transition Rules (`norca/norca/admission/object.go`)
Beyond structural JSON Schema checks, `ObjectKind.Rules` enables stateful invariant and transition enforcement using Google CEL (`ObjectRuleEngine`, `norca/norca/admission/object.go:46`):

- **Variables Exposed to CEL**:
  - `self`: The candidate object after defaulting (`{"kind", "metadata", "spec", "status"}`), where `metadata` exposes `name`, `namespace`, `labels`, `managedBy`, `generation`, `createIndex`, `modifyIndex`, `finalizers`, `deleting`, and `kindVersion` (`object_endpoint.go:896-928`).
  - `oldSelf`: The currently stored object on updates, or `null` on creation. A custom AST macro extends `has(oldSelf)` (`object.go:81-98`) so expressions can cleanly guard transition rules with `!has(oldSelf) || ...`.
  - `request`: Context map containing `operation` (`"create"`, `"update"`, `"status"`, `"scale"`, or `"migrate"`), `namespace`, `dryRun`, and `user` (`accessorId`, `name`, `type`, `roles`, `policies`).
- **Unit-Aware `quantity()` Function**:
  - `quantityFunction()` (`object.go:102-168`) registers a custom CEL overload `quantity(string|int|double) -> double` supporting SI decimal (`k`, `M`, `G`, `T`, `P`, `E`), IEEE 1541 binary (`Ki`, `Mi`, `Gi`, `Ti`, `Pi`, `Ei`), and milli (`m`) suffixes with optional trailing `B` (e.g., `quantity("100GiB") >= quantity(oldSelf.spec.storage)`).
- **Fail-Closed Evaluation**:
  - Every rule expression must type-check to `bool` and runs under strict CEL cost and timeout limits. Any runtime evaluation error counts as a rule violation (`object.go:221`).

### 2.4 Custom Printer Columns & Scale Subresource Path Mapping
- **Printer Columns (`ObjectKindColumn`)**: Each column maps a display header `Name` to an object field `Path` (`norca/norca/structs/objects.go:180-183`). The client evaluator (`api.CustomObject.Field`) supports dotted traversal (`spec.version`, `status.addresses`) as well as JSONPath-style predicate filtering over slices (`status.conditions[?type=='Ready'].status`), rendered automatically by `norca object list` and `norca object get` (`norca/command/object.go:383-412`).
- **Scale Subresource (`ObjectKindScale`)**: Declares `SpecPath` (e.g., `"spec.replicas"`) and optional `StatusPath` (e.g., `"status.readyReplicas"`). When configured:
  - `GET /v1/object/<group>/<kind>/<name>/scale` (`CustomObject.GetScale`, `object_endpoint_lifecycle.go:538`) extracts the desired and observed integer replica counts via `structs.ScaleOf(kind, obj)`.
  - `PUT /v1/object/<group>/<kind>/<name>/scale` (`CustomObject.Scale`, `object_endpoint_lifecycle.go:597`) mutates `SpecPath` via `structs.SetObjectPathValue`, increments `Generation`, bypasses `managed_by` restrictions (so the Nomad Autoscaler can scale GitOps-managed objects), and appends a `"Scaled"` event with `count` and `previous_count` metadata to the object's event log for autoscaler cooldown calculation.

### 2.5 Schema Versioning, CEL Migrations & Ecosystem Load Limits (`norca/norca/object_ecosystem.go`)
Norca provides zero-downtime schema evolution and multi-tenant protection directly on the `ObjectKind`:

1. **Pre-Flight Migration Verification (`checkStoredObjects`)**:
   - When `ObjectKind.UpsertObjectKind` modifies the schema, rules, migration, or `Version` (`Version` can never decrease), the leader snapshots MemDB and runs `migrateObject()` across **every existing object of that kind** (`object_ecosystem.go:643-713`).
   - If any stored object fails migration or fails the new JSON Schema, the `ObjectKind` update is rejected with `409 Conflict` and a detailed `ObjectKindMigrationReport` showing up to 20 failures (`MaxObjectMigrationFailures`) and 3 before/after diffs (`MaxObjectMigrationSamples`). Running `norca object-kind apply -dry-run` previews this exact migration report without mutating state.
2. **Dual-Path Object Migration (Background Loop + Migrate-on-Write)**:
   - **CEL Migration Engine (`norca/norca/admission/migration.go:47`)**: Evaluates `migration.spec` and `migration.status` CEL expressions with `self`, `fromVersion`, and `toVersion`, providing map helper methods `m.merge({...})` and `m.omit(["key"])` (`migration.go:93-121`).
   - **Lazy Migrate-on-Write (`migrateOnWrite`, `object_ecosystem.go:337`)**: If a user or controller updates an object whose `KindVersion < kind.SchemaVersion()` before the background worker reaches it, `writeObject` transparently migrates the stored object in-memory before applying the write.
   - **Leader Background Worker (`objectMigrationLoop`, `object_ecosystem.go:815-898`)**: Every `5s` (`objectMigrationInterval`), the leader migrates up to `200` objects (`objectMigrationBatch`) with a `10ms` pause (`objectMigrationPause`) between Raft writes to prevent Raft log flooding. Failed migrations record a `Warning` event (`Reason: "MigrationFailed"`) on the object's event log.
3. **Type-Bound Admission Webhooks (`ObjectKindWebhook`)**:
   - Kinds can bind `default` (mutating `/spec` via RFC 6902 JSON Patch) and `validate` webhooks (`object_ecosystem.go:455-558`).
   - **Cryptographic Approval Gate**: A webhook is only invoked if its SHA-256 configuration digest (`w.Digest()`, `objects_ecosystem.go:346`) matches an active `ObjectKindWebhookApproval`. Kinds applied with a management token are auto-approved; kinds applied by non-management operators remain pending until a cluster administrator runs `norca object-kind webhook approve <group>/<Kind> <webhook>` (`object_ecosystem.go:578-602`).
4. **Per-Kind & Per-Identity Rate Limiting (`ObjectKindLimits`)**:
   - Enforced in `objectEcosystem.allowWrite` and `acquireWatch` (`object_ecosystem.go:147-225`):
     $$\text{Allowed}(\text{req}) = \text{TokenBucket}_{\text{identity}}\!\left(r_{\text{id}}, b_{\text{id}}\right) \land \text{TokenBucket}_{\text{kind}}\!\left(r_{\text{all}}, b_{\text{all}}\right)$$
   - The per-identity limiter (`IdentityWriteRate`, up to `4096` active identity buckets evicted after `10m` idle) is checked **before** the global kind limiter (`WriteRate`), ensuring a single runaway controller cannot exhaust the write budget of other users. Exceeding any limit returns HTTP `429 Too Many Requests` (`structs.NewObjectLimitError`), which the Go Controller SDK recognizes as `ErrThrottled`.

---

## 3. Custom Object Lifecycle, Finalizers & Cascading Garbage Collection

### 3.1 `spec` vs. `status` Separation & Optimistic Concurrency Control
Every `CustomObject` (`norca/norca/structs/objects.go:326`) separates desired state from observed state across distinct RPCs, ACL capabilities, and generation counters:

| Aspect | Desired State (`CustomObject.Apply`) | Observed State (`CustomObject.UpdateStatus`) |
| :--- | :--- | :--- |
| **HTTP Endpoint** | `PUT /v1/object/:group/:kind/:name` | `PUT /v1/object/:group/:kind/:name/status` |
| **RPC Handler** | `CustomObject.Apply` (`object_endpoint.go:964`) | `CustomObject.UpdateStatus` (`object_endpoint.go:1081`) |
| **Required ACL** | `object "<group>/<Kind>" { capabilities = ["write"] }` (+ `manage-finalizers` if adding finalizers) | `object "<group>/<Kind>" { capabilities = ["write-status"] }` |
| **Mutated Fields** | `Spec`, `Labels`, `ManagedBy`, `Owners`, additive `Finalizers` | `Status` only (`Spec`, `Metadata`, `Finalizers` preserved) |
| **`Generation` Impact** | Initialized to `1` on create; incremented (`Generation++`) if and only if normalized `Spec` JSON changed (`object_endpoint.go:1070`) | Never changes `Generation`. Validates `status.observedGeneration <= obj.Generation` (`object_endpoint.go:1139`) |
| **`managed_by` Check** | Enforced (`checkManaged = true`, `object_endpoint_lifecycle.go:38`) | Not enforced (controllers have independent identities) |
| **Event Stream Type** | `CustomObjectUpserted` | `CustomObjectStatusUpdated` (`state_store_objects.go:549`) |

#### Optimistic Concurrency & Automatic Conflict Retry (`writeObject`):
In `CustomObject.writeObject` (`norca/norca/object_endpoint.go:649-812`), every Raft state mutation is submitted as a `CustomObjectUpsertStateRequest` carrying `ExpectedModifyIndex = existing.ModifyIndex` (`0` if creating). Inside `StateStore.UpsertCustomObject` (`norca/norca/state/state_store_objects.go:393-435`), MemDB verifies that the stored object's `ModifyIndex` still equals `ExpectedModifyIndex`, returning `409 Conflict` (`errObjectConflictPrefix = "object modified concurrently"`) on mismatch:
- **Explicit CAS (`-cas <index>`)**: Immediately returns `409 Conflict` to the caller if `existing.ModifyIndex != *w.cas`.
- **Implicit Retry Loop (`w.cas == nil`)**: If a concurrent write (e.g., a controller updating `/status` while a user runs `object apply`) changes `ModifyIndex` between snapshot read and Raft apply, `writeObject` automatically retries up to `objectWriteAttempts = 10` times with randomized linear-bounded backoff:
  $$T_{\text{sleep}}(\text{attempt}) \sim \text{Uniform}\!\left(0,\; \text{attempt} \times 5\text{ ms}\right)$$
- **No-Op Short-Circuit**: If `objectUnchanged(existing, obj)` (`object_endpoint.go:932`) detects that `Generation`, `SchemaVersion`, `ManagedBy`, `DeletionTime`, `Finalizers`, `Labels`, `Owners`, `Spec`, and `Status` are all byte-for-byte identical, the server returns `reply.Unchanged = true` without submitting a Raft log entry.

### 3.2 Identity Ownership Protection (`managed_by`)
Both Custom Objects (`CustomObject.ManagedBy`) and Nomad Jobs (`Job.Meta["norca.managed-by"]`) support cryptographic identity locking via `structs.ManagedByAllows` (`norca/norca/structs/objects_lifecycle.go:426-442`):
- **`workload:<namespace>/<job>[/<task>]`**: Only a task presenting a Norca Workload Identity JWT for that job/task may update or delete the resource.
- **`role:<name>` / `policy:<name>`**: Only an ACL token possessing the named role or policy may modify the resource.
- **`token:<accessor_id>`**: Locked to a specific ACL token accessor ID.
- Management tokens bypass `managed_by` restrictions for emergency operator intervention.

### 3.3 Finalizers & Pre-Deletion Lifecycle (`object_endpoint_lifecycle.go`)
Finalizers (`CustomObject.Finalizers`, up to 32 strings matching `^[a-zA-Z0-9]([a-zA-Z0-9._/-]{0,126}[a-zA-Z0-9])?$`) prevent premature deletion of objects that represent external or stateful resources:

1. **Adding Finalizers**:
   - Added either during `CustomObject.Apply` or via `CustomObject.UpdateFinalizers` (`PUT /v1/object/:group/:kind/:name/finalizers`, `object_endpoint_lifecycle.go:315`), requiring the `manage-finalizers` ACL capability.
   - Finalizers **cannot** be added once an object is marked for deletion (`obj.Deleting() == true` returns `409 Conflict: "... is being deleted; finalizers can't be added"`, `object_endpoint_lifecycle.go:30-34`).
2. **Deletion State Transition (`CustomObject.Delete`)**:
   - When `CustomObject.Delete` (`object_endpoint_lifecycle.go:174-310`) is invoked, the server evaluates:
     $$\text{removeNow} = \left(|\text{existing.Finalizers}| == 0\right) \land \left(\text{propagation} \neq \text{"foreground"}\right)$$
   - If `removeNow` is `true`, the object is immediately deleted from `TableCustomObjects` via `removeCustomObject()` (`CustomObjectDeleteRequestType`), which atomically deletes its event log and writes an `ObjectTombstone` (unless `propagation == "orphan"`).
   - If `removeNow` is `false` (the object has finalizers or uses `foreground` propagation), the object is **not** removed from MemDB. Instead, `DeletionTime` is set to `time.Now().UnixNano()` and `DeletionPropagation` is recorded via a metadata-only `CustomObjectUpsertRequestType`. Note that `metadataOnly: true` (`object_endpoint.go:634`) skips schema and CEL validation during finalizer removal and deletion marking so that an outdated schema can never trap an object in an undeletable state.
3. **Finalizer Removal & Immediate Finalization**:
   - Controllers watching `TopicCustomObject` observe `obj.Deleting() == true`, execute external cleanup, and call `UpdateFinalizers` (`Remove: ["..."]`).
   - When the last finalizer is removed, `UpdateFinalizers` invokes `s.finalizeObjectIfReady(reply.Object)` (`object_endpoint_lifecycle.go:133-146`, `378`): if `len(obj.Finalizers) == 0` and `DeletionPropagation != "foreground"`, the server immediately removes the object from `TableCustomObjects`. If a concurrent write raced or `foreground` propagation is active, `objectgc.Collector` completes the removal.

### 3.4 Owner References & Cascading Garbage Collection (`norca/norca/objectgc/`)
Norca implements a full dependency graph collector (`objectgc.Collector`, `norca/norca/objectgc/objectgc.go:94`, wired to the server leader in `norca/norca/objectgc_server.go:15`) that manages two classes of dependent resources:

1. **Dependent Custom Objects (`CustomObject.Owners`)**:
   - A `CustomObject` can declare up to 16 `ObjectOwnerReference` entries (`Kind: "<group>/<Kind>"`, `Namespace`, `Name`, `ID`).
   - On `Apply`, `resolveObjectOwners()` (`object_endpoint_lifecycle.go:52-93`) automatically defaults `Namespace` to the child object's namespace (or `""` if the owner's `ObjectKind` is cluster-scoped) and populates `ID` with the current UUID of the owner if it exists.
   - Multi-owner semantics (`ownersGone`, `objectgc.go:378-395`): A dependent object is garbage-collected **only when all of its custom object owners are deleted**. If an object has two owners $O_1$ and $O_2$, deleting $O_1$ leaves the dependent intact until $O_2$ is also deleted.
2. **Dependent Nomad Jobs (`Job.Meta["norca.owner"]`)**:
   - Controllers mark child Nomad jobs using `controller.SetOwner(job, obj)` (`norca/controller/owner.go:31`), which sets:
     - `meta["norca.owner"] = "<namespace>/crd/<group-with-dashes>/<Kind>/<name>"` (`structs.JobMetaOwner`)
     - `meta["norca.owner.kind"] = "<group>/<Kind>"` (`structs.JobMetaOwnerKind`)
   - When an owner is collected, `gcStopJob` (`norca/norca/objectgc_server.go:55-67`) invokes `Job.Deregister` (`Purge: false`) using `s.getLeaderAcl()`, which bypasses `norca.managed-by` job protection and cleanly stops the job's allocations.

```
                  norca object delete db.example.com/PostgresCluster billing
                                              |
        +-------------------------------------+-------------------------------------+
        |                                     |                                     |
        v (-propagation=background)           v (-propagation=foreground)           v (-propagation=orphan)
+-------------------------------+     +-------------------------------+     +-------------------------------+
|     BACKGROUND (Default)      |     |          FOREGROUND           |     |            ORPHAN             |
|                               |     |                               |     |                               |
| 1. If Finalizers == 0:        |     | 1. Parent stays in MemDB with |     | 1. Once Finalizers == 0:      |
|    Parent removed from        |     |    DeletionTime > 0 &         |     |    Parent removed from        |
|    custom_objects immediately.|     |    Propagation="foreground".  |     |    custom_objects.            |
| 2. ObjectTombstone written    |     | 2. objectgc.Collector sweeps: |     | 2. NO ObjectTombstone is      |
|    to object_tombstones.      |     |    - Deletes child Objects    |     |    written to MemDB.          |
| 3. objectgc.Collector sweeps  |     |    - Stops owned Jobs & waits |     | 3. Child CustomObjects and    |
|    child Objects & stops      |     |      until Job.Status=="dead" |     |    owned Nomad Jobs remain    |
|    owned Jobs asynchronously. |     | 3. Once remaining == 0 and    |     |    running untouched (can be  |
| 4. Tombstone kept for 5m      |     |    Finalizers == 0, parent    |     |    adopted later via          |
|    (TombstoneRetention) to    |     |    is removed & Tombstone     |     |    controller.AdoptJobs()).   |
|    catch late-racing children.|     |    written for 5m retention.  |     |                               |
+-------------------------------+     +-------------------------------+     +-------------------------------+
```

#### Tombstone Retention & Resurrection Adoption (`objectgc.go:313-334`):
- Why keep `ObjectTombstone` records (`TableObjectTombstones = "object_tombstones"`) for `TombstoneRetention = 5 * time.Minute` (`objectgc.go:61`) after all known dependents are gone?
  - In a distributed system, a controller that has not yet processed the parent's deletion event might be mid-reconcile and create a child job or child object *milliseconds after* the parent was deleted from `TableCustomObjects`. Because the tombstone persists for 5 minutes and `objectgc.Collector` wakes on MemDB watches (`Interval = 5s`, `IdleInterval = 1m`), late-created orphans are still caught and swept.
- **Resurrection / Re-Creation Guard (`objectgc.go:318-321`)**:
  - Before collecting dependents of a tombstone `o`, the collector checks `if _, exists := w.objects[o.ref()]; !exists`. If a user re-creates or restores an object with the same `group/Kind`, `namespace`, and `name` while the tombstone is still in its retention window, garbage collection for that tombstone is immediately halted so the re-created owner seamlessly adopts its existing jobs and child objects.

---

## 4. The Norca Controller & Operator SDK (`norca/controller/`)

The `norca/controller` package provides an idiomatic, Kubernetes `controller-runtime`-inspired Go framework tailored for Norca. It depends solely on `github.com/bacanoo-io/norca/norca/api` and abstracts all cluster interactions behind the `Backend` interface (`norca/controller/backend.go`), allowing operators to run identically against production clusters, real dev agents (`norca/controller/envtest`), or deterministic in-memory unit-test fakes (`norca/controller/fake`).

### 4.1 Dual Storage Abstraction (`StorageNative` vs. `StorageVariables`)
To support both modern Norca clusters and backward-compatible Nomad 1.6.5 environments, `controller.Options.Storage` (`norca/controller/native.go:18-30`) supports two storage backends behind the unified `*controller.Object` struct:

| Feature | `StorageNative` (`"native"`) | `StorageVariables` (`""` or `"variables"`) |
| :--- | :--- | :--- |
| **Backing Store** | `TableCustomObjects` (`CustomObject` RPCs) | `TableVariables` at path `crd/<dashed-group>/<Kind>/<name>` |
| **Schema & CEL Validation** | Enforced server-side by `ObjectKind` | Validated client-side by reconciler / webhook |
| **Status Endpoint & ACL** | Separate `/status` endpoint (`write-status` capability) | Stored in `"status"` item of the same Variable (`write` capability) |
| **Generation Tracking** | Server-maintained `obj.Generation` vs. `status.observedGeneration` | SHA-256 canonical JSON digest `obj.SpecHash()` (16 hex chars) vs. `status.observedSpecHash` |
| **Finalizers & Server GC** | Native `Finalizers`, `DeletionTime`, `Owners`, and `objectgc` | Cleaned up on `OnDelete` reconcile (`!ctrl.Get(key)`) |
| **Object Event Log** | Native ring buffer (`Controller.Recorder()`) | No-op |
| **Event Stream Topic** | `TopicCustomObject` (carries full `*api.CustomObject` payload) | `TopicVariables` (carries metadata; informer fetches Variable on change) |

### 4.2 Informer Cache & Exact-Index Event Stream Watcher (`informer.go`, `watch.go`)
Each `Controller` runs an `Informer` (`norca/controller/informer.go:52`) managed by `watcher.run()` (`norca/controller/watch.go:127`):
1. **Initial List & Sync**: Calls `store.list(ctx, namespace, 0)`, populates the thread-safe `cache map[ObjectKey]*Object`, fires `OnAdd` handlers, and closes the `synced` channel (`WaitForSync`).
2. **Exact Resumption Event Streaming**: Subscribes to `/v1/event/stream` starting at `next = listIndex + 1`. For `StorageNative`, `CustomObjectUpserted`, `CustomObjectStatusUpdated`, and `CustomObjectDeleted` events contain the complete object payload (`state_store_objects.go:558`), so the `Informer` updates its local cache in $O(1)$ **without issuing any follow-up HTTP GET requests** (`informer.go:316-319`).
3. **Automatic `410 Gone` & Disconnect Recovery**:
   - If the network drops, `watcher.run()` reconnects at the exact `next` Raft index so zero events are missed.
   - If the server's event buffer has rotated past `next` and returns `410 Gone` (`api.ErrEventIndexGone`), the watcher logs a resync metric (`reason = "gone"`) and performs a full relist (`watch.go:199-202`).
4. **Workload Identity & Polling Fallback (`WatchAuto`)**:
   - In `WatchAuto` mode (default), if the server rejects `/v1/event/stream` with `403 Forbidden` (or `500` on older servers), the watcher automatically falls back to blocking queries (`waitIndex = next - 1`) without crashing the operator (`watch.go:203-206`).
5. **Default `SpecChanged` Predicate (`controller.go:45-47`)**:
   - Filters `OnUpdate` events using:
     $$\text{SpecChanged}(\text{old}, \text{new}) = \neg\text{specEqual}(\text{old}, \text{new}) \lor \text{lifecycleChanged}(\text{old}, \text{new}) \lor |\text{new.Status}| == 0$$
   - Because a controller's own `UpdateStatus` write changes only `Status` (and `ModifyIndex`), `SpecChanged` evaluates to `false`, preventing infinite self-reconcile loops.

### 4.3 Deduplicating Workqueue & Composite Rate Limiting (`queue.go`, `ratelimit.go`)
Keys enqueued by the `Informer`, `OwnedJobsSource`, or custom `EventSource`s enter `Queue[ObjectKey]` (`norca/controller/queue.go:19`):
- **Strict Concurrency & Deduplication Invariant**:
  - The queue maintains three sets: `queue []K` (FIFO slice), `dirty map[K]struct{}`, and `processing map[K]struct{}`.
  - If key $k$ is added multiple times before a worker calls `Get()`, it appears in `queue` only once.
  - If key $k$ is currently in `processing` (being reconciled by a worker goroutine) and an event enqueues $k$ again, $k$ is marked in `dirty` but **never** dispatched to a second worker concurrently. When the active worker finishes and calls `q.Done(k)` (`queue.go:134-142`), $k$ is moved from `dirty` back to `queue`, guaranteeing serialized, level-triggered execution per object.
- **Composite Backoff (`DefaultRateLimiter`, `ratelimit.go:132-137`)**:
  - Combines per-key exponential backoff (`Base = 500ms`, `Max = 5m`) with an overall token bucket (`QPS = 10`, `Burst = 100`) via `MaxOf`:
    $$\Delta t_{\text{retry}}(k, n) = \max\!\left(\min\!\left(500\text{ ms} \times 2^n,\; 5\text{ m}\right),\; \Delta t_{\text{bucket}}(10\text{ QPS}, 100)\right)$$
  - A successful reconcile (`err == nil`) calls `q.Forget(key)` to reset $n = 0$. If `reconcile` returns `(requeueAfter > 0, nil)`, the key is scheduled via `q.AddAfter(key, requeueAfter)`. If the server returns `429 Too Many Requests` (`IsThrottled(err)`), the controller records `ResultThrottled` and backs off cleanly without spamming error logs (`controller.go:373-380`).

### 4.4 Clock-Skew-Tolerant Distributed Leader Election (`leader.go`)
When running multiple replicas of an operator for high availability, `LeaderElector` (`norca/controller/leader.go:106`) ensures only the active leader runs the workqueue and event sources, while **all replicas run their `Informer`s continuously** so standby replicas maintain a warm cache for instant failover (`controller.go:256-290`).

- **CAS Lease Variable**: Uses a Nomad Variable at `controller-lease/<controller-name>` containing `holder`, `leaseDurationSeconds` (default `20s`), `acquireTime`, `renewTime`, and `transitions` (`leader.go:24-33`).
- **Monotonic Local-Clock Expiry (Zero Clock-Skew Sensitivity)**:
  - Standby candidates **do not** compare the leader's wall-clock `renewTime` string against their own wall clock (which would break under NTP drift).
  - Instead, each candidate records `le.observedTime = le.now()` using its **own local clock** whenever it observes `v.ModifyIndex != le.observedIndex` (`leader.go:299-301`).
  - A standby candidate takes over the lease via CAS (`ModifyIndex: v.ModifyIndex`) if and only if:
    $$\text{holder} = \emptyset \;\lor\; \text{holder} = \text{self} \;\lor\; t_{\text{local\_now}} \ge t_{\text{local\_observed}}(\text{ModifyIndex}) + D_{\text{lease}}$$
- **Graceful Step-Down**: When `ReleaseOnCancel: true` is set and the controller receives `SIGINT`/`SIGTERM`, `le.release()` (`leader.go:342-364`) clears the `holder` item via CAS so a standby replica acquires leadership on its very next `RetryPeriod` (`4s`) tick instead of waiting the full `20s` `LeaseDuration`.

### 4.5 Status Conditions, Owned Jobs & Autoscaler Count Preservation
- **`CommonStatus` & `Condition` (`status.go:26-115`)**:
  - Standard condition types: `ConditionReady` (`"Ready"`), `ConditionProgressing` (`"Progressing"`), `ConditionDegraded` (`"Degraded"`).
  - `status.Observe(obj)` sets `ObservedGeneration = obj.Generation` (for native objects) or `ObservedSpecHash = obj.SpecHash()` (for Variables).
  - `status.SetCondition(cond, now)` updates a condition in place and preserves `LastTransitionTime` whenever `Status` (`"True"`, `"False"`, `"Unknown"`) has not changed.
  - `Objects().UpdateStatus(ctx, obj, status)` (`objects.go:127-152`) skips the RPC if the JSON status is unchanged, and automatically re-reads the object and retries up to `maxStatusRetries = 5` times on `409 Conflict`.
- **`OwnedJobsSource` (`owner.go:176-332`)**:
  - Enabled via `Options{OwnsJobs: true}`. Maintains an in-memory map `namespace/jobID -> ObjectKey` of jobs carrying `meta["norca.owner"]` and watches `TopicJob`, `TopicAllocation`, `TopicEvaluation`, and `TopicDeployment`, immediately enqueueing the owning `CustomObject` whenever any allocation or deployment of an owned job changes state.
- **`PreserveScaledCounts(desired, existing)` (`lifecycle.go:274-306`)**:
  - If a child job managed by an operator has a task group with its own active `scaling` block (targeting the task group itself rather than a custom object), `PreserveScaledCounts` copies the live `tg.Count` from `existing` into `desired` before `RegisterJob`, preventing the operator from overwriting horizontal autoscaler decisions.

---

## 5. Real-World Operator Examples in the Norca Ecosystem

### 5.1 `loadbalancer-operator` (`norca/controller/examples/loadbalancer/`)
The reference operator in `norca/controller/examples/loadbalancer/` implements `norca.io/LoadBalancer`, bringing Kubernetes `Service` `type: LoadBalancer` semantics to Nomad services:
1. **Dual-Controller Architecture (`main.go:225-266`)**:
   - **Primary Controller (`loadbalancer`)**: Reconciles `norca.io/LoadBalancer` objects (`Workers: 2`, `OwnsJobs: true`, plus a custom `serviceSource()` watching `TopicService`).
   - **Discovery Controller (`loadbalancer-discovery`, `discovery.go:194`)**: Watches `TopicJob` for services annotated with `meta { norca_lb_class = "nomad", norca_lb_port = "443" }` and automatically creates, updates, and deletes corresponding `norca.io/LoadBalancer` objects labeled `managed-by = "service-mark"`.
2. **Reconciliation & Finalizer Workflow (`reconcile.go:38-157`)**:
   - On creation of a native `LoadBalancer`, registers finalizer `"norca.io/loadbalancer-cleanup"` via `AddFinalizer`.
   - Queries healthy service instances via `ServiceTargets(ctx, namespace, spec.Service)`.
   - Invokes `nomadProvider.Ensure()` (`provider.go:129-164`), which writes the live target endpoints to the proxy job's workload-identity Variable (`norca/jobs/lb-<name>`) and registers the TCP proxy job `lb-<name>` with `controller.SetOwner(job, obj)` and `controller.SetManagedBy(job, "workload:default/loadbalancer-controller")`. Because target IP changes only update the Variable `norca/jobs/lb-<name>`, the proxy task reloads targets dynamically without re-scheduling the Nomad job.
   - Updates `Ready`, `Progressing`, and `Degraded` conditions via `UpdateStatus` and emits deduplicated events (`"Ready"`, `"NoHealthyTargets"`, `"ProvisioningFailed"`) via `Recorder().Event()`.
   - When deleted (`obj.Deleting() == true`), `r.cleanup()` purges the proxy job and Variable before calling `RemoveFinalizer`.

### 5.2 Built-In Package Manager Releases (`packs.norca.io/Release` & `ReleaseRevision`)
Norca's server-side package manager (`norca/norca/release_kinds.go:90-125`) dogfoods the Custom Object engine directly:
- Registers two built-in `ObjectKind`s on first release use:
  - **`packs.norca.io/Release`**: Tracks the active release specification (`pack`, `revision`, `atomic`, `multiregion`) and live status (`phase`, `resources`, `Drifted` condition), with custom printer columns `Pack`, `Version`, `Revision`, `Phase`, and `Drifted`.
  - **`packs.norca.io/ReleaseRevision`**: Stores each historical revision (`<release>.v<N>`) with a CEL immutability rule (`!has(oldSelf) || self.spec == oldSelf.spec`) and an `ObjectOwnerReference` pointing to its parent `packs.norca.io/Release` (`release_kinds.go:340-345`).
- Direct writes to `packs.norca.io/*` via the generic `CustomObject` endpoint are blocked by `isBuiltinObjectGroup` (`release_kinds.go:129`), while `norca object list packs.norca.io/Release`, `norca object get`, `norca object events`, and `TopicCustomObject` watches work out of the box.

### 5.3 Multi-Region Service Mesh & `norca operator init` Scaffolding
- While `mtls-gateway` (`example/04-multi-region/mtls-gateway.norca.hcl`) provides zero-trust SNI passthrough routing across federated regions using `service { mtls { mode = "gateway" } }`, platform teams building custom infrastructure controllers can scaffold a complete, production-ready operator repository in one command via `norca operator init` (`norca/command/operator_init.go:26`).
- `norca operator init db.example.com/PostgresCluster` generates:
  - The `ObjectKind` HCL definition (`kind.hcl`), JSON Schema, CEL rules, printer columns, and sample custom object.
  - The Go controller module (`main.go`, `reconciler.go`, `types.go`, `webhook.go`) wired with finalizers, `CommonStatus` conditions, leader election, and HTTPS validating webhooks.
  - Unit tests using `controller/fake` and integration tests using `controller/envtest`.
  - A ready-to-deploy Norca Pack (`pack/`) bundling the `ObjectKind`, least-privilege `ACLPolicy`, and controller `Job` into an atomic release.

---

## 6. HCL, JSON Schema, ACL & Go SDK Reference

### 6.1 `ObjectKind` HCL Definition (`postgrescluster.kind.hcl`)
```hcl
kind "PostgresCluster" {
  group       = "db.example.com"
  description = "High-availability PostgreSQL cluster managed by postgres-operator"
  namespaced  = true
  version     = 2
  schema      = file("postgrescluster.schema.json")

  # CEL validation and transition rules
  rule {
    expression = "self.spec.replicas >= 1 && self.spec.replicas <= 7 && self.spec.replicas % 2 == 1"
    message    = "replicas must be an odd number between 1 and 7"
  }

  rule {
    expression = "!has(oldSelf) || quantity(self.spec.storage) >= quantity(oldSelf.spec.storage)"
    message    = "storage size cannot be decreased once provisioned"
  }

  # In-place CEL schema migration from version 1 -> 2
  migration {
    spec = "self.spec.omit(['diskSize']).merge({'storage': self.spec.diskSize})"
  }

  # Scale subresource mapping for `norca object scale` and the Autoscaler
  scale {
    spec_path   = "spec.replicas"
    status_path = "status.readyReplicas"
  }

  # Custom CLI & UI printer columns
  columns {
    column "Version"  { path = "spec.version" }
    column "Storage"  { path = "spec.storage" }
    column "Replicas" { path = "status.readyReplicas" }
    column "Primary"  { path = "status.primaryEndpoint" }
    column "Ready"    { path = "status.conditions[?type=='Ready'].status" }
  }

  # Type-bound HTTPS validating webhook
  webhook "validate" {
    mode           = "validate"
    url            = "https://postgres-operator.service.consul:8443/validate"
    ca_cert        = file("webhook-ca.pem")
    timeout        = "5s"
    failure_policy = "fail"
    operations     = ["create", "update", "delete"]
  }

  # Server-side token bucket & watch protection
  limits {
    write_rate                = 50
    write_burst               = 100
    identity_write_rate       = 10
    identity_write_burst      = 20
    max_objects_per_namespace = 100
    max_watches_per_identity  = 10
  }
}
```

### 6.2 `CustomObject` HCL Specification (`billing-db.hcl`)
```hcl
kind       = "db.example.com/PostgresCluster"
name       = "billing"
namespace  = "prod"
managed_by = "role:platform-gitops"

labels = {
  team        = "finance"
  environment = "production"
}

spec {
  version  = "16.4"
  replicas = 3
  storage  = "200GiB"
  backup {
    schedule = "0 2 * * *"
  }
}
```

### 6.3 Least-Privilege Operator ACL Policy (`postgres-operator-policy.hcl`)
Bind this policy directly to the operator job's Workload Identity (`norca acl policy apply -namespace default -job postgres-operator postgres-operator postgres-operator-policy.hcl`) so no static secret token is needed:

```hcl
namespace "*" {
  capabilities = ["list-jobs", "read-job", "submit-job"]

  # Grant status, event, and finalizer privileges without needing spec write on user objects
  object "db.example.com/PostgresCluster" {
    capabilities = ["read", "write-status", "write-events", "manage-finalizers"]
  }

  variables {
    # Leader election lease variable
    path "controller-lease/postgres-operator" {
      capabilities = ["read", "write"]
    }
  }
}
```

### 6.4 Minimal Go Operator Implementation (`norca/controller/`)
```go
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bacanoo-io/norca/norca/controller"
)

const finalizerName = "db.example.com/postgres-cleanup"

var pgKind = controller.Kind{Group: "db.example.com", Kind: "PostgresCluster"}

type Spec struct {
	Version  string `json:"version"`
	Replicas int    `json:"replicas"`
	Storage  string `json:"storage"`
}

type Status struct {
	controller.CommonStatus
	ReadyReplicas   int    `json:"readyReplicas"`
	PrimaryEndpoint string `json:"primaryEndpoint,omitempty"`
}

type Reconciler struct {
	ctrl *controller.Controller
}

func (r *Reconciler) Reconcile(ctx context.Context, key controller.ObjectKey) (time.Duration, error) {
	obj, ok := r.ctrl.Get(key)
	if !ok {
		r.ctrl.Recorder().Forget(key)
		return 0, nil
	}

	// 1. Handle deletion & finalizer cleanup
	if obj.Deleting() {
		if obj.HasFinalizer(finalizerName) {
			// Perform external cleanup before releasing the finalizer
			_, err := r.ctrl.Objects().RemoveFinalizer(ctx, obj, finalizerName)
			return 0, err
		}
		return 0, nil
	}
	if !obj.HasFinalizer(finalizerName) {
		next, err := r.ctrl.Objects().AddFinalizer(ctx, obj, finalizerName)
		if err != nil {
			return 0, err
		}
		obj = next
	}

	// 2. Decode spec & prepare status
	var spec Spec
	if err := obj.DecodeSpec(&spec); err != nil {
		return 0, err
	}
	var status Status
	_ = obj.DecodeStatus(&status)
	status.Observe(obj)

	// 3. Reconcile child Nomad jobs (carrying controller.SetOwner(job, obj))
	status.ReadyReplicas = spec.Replicas
	status.PrimaryEndpoint = key.Name + "-primary.service.consul:5432"
	status.SetCondition(controller.Condition{
		Type:    controller.ConditionReady,
		Status:  controller.ConditionTrue,
		Reason:  "ClusterHealthy",
		Message: "All PostgreSQL instances are streaming and healthy",
	}, time.Now())

	// 4. Write status subresource with automatic CAS retry & emit event
	if _, err := r.ctrl.Objects().UpdateStatus(ctx, obj, status); err != nil {
		return 0, err
	}
	r.ctrl.Recorder().Event(ctx, key, controller.EventTypeNormal, "Reconciled", "Cluster reconciled")
	return 0, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, _ := controller.NewAPIClient()
	backend := controller.NewBackend(client)
	rec := &Reconciler{}
	ctrl, _ := controller.New(backend, rec, controller.Options{
		Name:      "postgres-operator",
		Kind:      pgKind,
		Storage:   controller.StorageNative,
		Namespace: "*",
		Workers:   2,
		OwnsJobs:  true,
		LeaderElection: &controller.LeaderElectionConfig{
			Namespace:       "default",
			ReleaseOnCancel: true,
		},
	})
	rec.ctrl = ctrl
	_ = ctrl.Run(ctx)
}
```

---

## 7. CLI & HTTP API Cheat-Sheet

### 7.1 CLI Commands (`norca object-kind`, `norca object`, `norca operator init`)

| Command | Action | Key Flags |
| :--- | :--- | :--- |
| `norca object-kind apply <file>` | Register or update an `ObjectKind` schema, CEL rules, webhooks, and limits | `-dry-run` (shows migration report), `-json` |
| `norca object-kind list` | List all registered `ObjectKind`s visible to the caller | `-prefix <group/>`, `-json`, `-t` |
| `norca object-kind inspect <group>/<Kind>` | Inspect schema, CEL rules, columns, migration, limits, and webhook approvals | `-json`, `-t` |
| `norca object-kind delete <group>/<Kind>` | Delete an `ObjectKind` (rejected with `409` if objects of that kind still exist) | - |
| `norca object-kind lint <kind.hcl> [samples...]` | Offline validation of schema, defaults, CEL rules, migrations, and sample objects | `-show-defaults`, `-json` |
| `norca object-kind webhook approve <g>/<K> <wh>`| Cryptographically approve a type-bound webhook (requires management token) | - |
| `norca object-kind webhook revoke <g>/<K> <wh>` | Revoke approval of a type-bound webhook | - |
| `norca object apply <file>` | Create or update the `spec` and metadata of a `CustomObject` | `-cas <idx>`, `-dry-run`, `-json` |
| `norca object list <group>/<Kind>` | List objects of a kind using its custom printer columns | `-namespace=*`, `-label k=v`, `-filter`, `-per-page` |
| `norca object get <group>/<Kind> <name>` | Display metadata, printer columns, conditions table, spec, status, and recent events | `-namespace`, `-json`, `-t` |
| `norca object delete <group>/<Kind> <name>` | Delete a custom object (or mark `Deleting` if finalizers exist) | `-propagation=background\|foreground\|orphan`, `-cas` |
| `norca object events <group>/<Kind> <name>` | Inspect the deduplicated per-object event log (`Normal` / `Warning`) | `-namespace`, `-json` |
| `norca object scale <g>/<K>/<name> [<count>]` | Read or set the `/scale` subresource replica count | `-message <reason>`, `-cas <idx>`, `-json` |
| `norca operator init <group>/<Kind> [<dir>]` | Scaffold a complete Go operator project, webhook, `envtest` suite, and pack | `-name`, `-module`, `-cluster-scoped`, `-sdk`, `-force` |

### 7.2 HTTP API Endpoints (`norca/command/agent/object_endpoint*.go`)

| HTTP Method & Path | RPC Handler |Required ACL Capability |
| :--- | :--- | :--- |
| `GET /v1/object-kinds` | `ObjectKind.List` | `operator:read` or any object capability on the kind |
| `GET /v1/object-kind/:group/:kind` | `ObjectKind.GetObjectKind` | `operator:read` or any object capability on the kind |
| `PUT,POST /v1/object-kind/:group/:kind` | `ObjectKind.UpsertObjectKind` | `operator:write` |
| `PUT,POST /v1/object-kind/:group/:kind/plan` | `ObjectKind.UpsertObjectKind` (`DryRun=true`) | `operator:write` |
| `DELETE /v1/object-kind/:group/:kind` | `ObjectKind.DeleteObjectKind` | `operator:write` |
| `PUT,POST /v1/object-kind/:group/:kind/webhook/:wh/approve` | `ObjectKind.ApproveWebhook` | Management token |
| `GET /v1/objects/:group/:kind?label=k=v` | `CustomObject.List` | `object:read` |
| `GET /v1/object/:group/:kind/:name` | `CustomObject.GetCustomObject` | `object:read` |
| `PUT,POST /v1/object/:group/:kind/:name` | `CustomObject.Apply` | `object:write` (+ `manage-finalizers` if adding) |
| `DELETE /v1/object/:group/:kind/:name?propagation=...` | `CustomObject.Delete` | `object:write` |
| `PUT,POST /v1/object/:group/:kind/:name/status` | `CustomObject.UpdateStatus` | `object:write-status` |
| `PUT,POST /v1/object/:group/:kind/:name/finalizers` | `CustomObject.UpdateFinalizers`| `object:manage-finalizers` |
| `GET /v1/object/:group/:kind/:name/events` | `CustomObject.ListEvents` | `object:read` |
| `PUT,POST /v1/object/:group/:kind/:name/events` | `CustomObject.RecordEvents` | `object:write-events` |
| `GET /v1/object/:group/:kind/:name/scale` | `CustomObject.GetScale` | `object:read` |
| `PUT,POST /v1/object/:group/:kind/:name/scale` | `CustomObject.Scale` | `object:write` |

---

## 8. Summary for Platform & Operator Developers

1. **Always Separate `write` and `write-status` Capabilities**: Grant application developers and GitOps pipelines `capabilities = ["read", "write"]` on custom objects, and bind `capabilities = ["read", "write-status", "write-events", "manage-finalizers"]` to the operator's Workload Identity. This guarantees controllers can never mutate user-declared `spec` fields and users cannot spoof controller `status` conditions.
2. **Use `norca object-kind lint` and `-dry-run` in CI/CD**: Before rolling out a new `ObjectKind` schema version, validate your schema defaults and CEL rules offline with `norca object-kind lint`, then run `norca object-kind apply -dry-run` against staging/production to verify that 100% of stored objects pass the CEL migration.
3. **Leverage `controller.SetOwner` and `controller.SetManagedBy` on Child Jobs**: Setting `SetOwner(job, obj)` enables automatic `OwnedJobsSource` reconciliation and server-side cascading garbage collection (`objectgc`), while `SetManagedBy(job, controller.ManagedByFromEnv())` prevents ad-hoc manual `norca job run` edits from causing configuration drift on operator-managed workloads.
4. **Call `PreserveScaledCounts` Before Registering Child Jobs**: If any task group in a child job includes an autonomous horizontal `scaling` policy, call `controller.PreserveScaledCounts(desiredJob, existingJob)` so your reconciler does not reset replica counts adjusted by the autoscaler.
5. **Configure `limits` on Every Production `ObjectKind`**: Always set `identity_write_rate`, `write_rate`, `max_objects_per_namespace`, and `max_watches_per_identity` in your `ObjectKind` definition to isolate Raft consensus capacity against buggy third-party reconciliation loops.
