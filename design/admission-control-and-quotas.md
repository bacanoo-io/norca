<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Admission Control Pipeline (CEL & Webhooks) and Resource Quota Architecture

## 1. Executive Summary & Core Architectural Principles

Norca provides a clean-room, open-standard admission control and resource quota subsystem (`norca/norca/admission/`, `norca/norca/quota_endpoint.go`, `norca/scheduler/quota.go`). By combining embedded Google Common Expression Language (`cel-go`), Kubernetes-style HTTP/HTTPS mutating and validating admission webhooks (`admission.norca.io/v1`), cosign-compatible cryptographic pack signature verification, and multi-dimensional regional quotas, Norca delivers defense-in-depth governance across every cluster write path.

### Key Architectural Tenets:
1. **Open-Standard Policy-as-Code (Google CEL)**: Policies are authored in standard Google CEL (`github.com/google/cel-go`), compiled into type-checked abstract syntax trees (ASTs) at policy registration time, cached in memory by Raft `ModifyIndex`, and executed with strict CPU cost and wall-clock time bounds.
2. **Non-Overridable Fail-Closed Enforcement**: Norca's dynamic admission policies (`action = "deny"`) and webhooks (`failure_policy = "fail"`) are strictly non-overridable: the `-policy-override` flag of `norca job run` does not bypass them. Any CEL evaluation error, missing field access, or webhook timeout under the default failure policy fails closed and rejects the write.
3. **Separation of Cluster-Wide Webhooks and Raft-Replicated Policies**: External HTTP/HTTPS webhooks are configured exclusively in the server configuration file (`norca.hcl`) so that a compromised API token can never register an external listener to exfiltrate cluster workloads or secrets. Declarative CEL `AdmissionPolicy` objects are stored in Raft (`TableAdmissionPolicies`) and managed via ACL-protected APIs (`admission:write`).
4. **Unified Multi-Resource Governance**: Admission control extends beyond job specifications to govern encrypted **Variables** (including Draft 2020-12 JSON Schema validation via `schema_from = "crd-schema"`), **Custom Objects** (Operator Framework kinds, CEL transition rules, schema migrations, and admin-approved type-bound webhooks), and **Pack Releases** (cosign public-key signature verification and release metadata inspection).
5. **Dual-Phase Resource Quotas & LimitRanges**: Resource quotas combine Kubernetes `LimitRange` semantics (`task_defaults` and `task_max` enforced during job admission) with `ResourceQuota` aggregate accounting (`cpu`, `memory`, `memory_max`, `allocations`, and `variables_limit`) enforced both at scheduler node selection (`QuotaIterator`) and authoritatively at Raft plan application (`evaluatePlanQuota`).

---

## 2. Synchronous Admission Control & Quota Pipeline

Every state-mutating request in Norca traverses a deterministic, synchronous pipeline on the region leader before a Raft log entry is committed.

### 2.1 End-to-End Job Submission, Scheduling, and Quota Pipeline

```
                        CLIENT / CLI / API REQUEST
                    (Job.Register or Job.Plan RPC)
                                  |
                                  v
+-----------------------------------------------------------------------------------+
| PHASE 1: MUTATING ADMISSION CHAIN (norca/norca/job_endpoint.go:71-84)             |
|                                                                                   |
|  1. Built-in Pre-Webhook Mutators:                                                |
|     ├── canonicalize            (Job defaults, server JobDefaultPriority)         |
|     ├── vault-namespace-default (Propagates Job.VaultNamespace to Task.Vault)     |
|     ├── scaling                 (Normalizes group/task scaling policies)          |
|     ├── connect                 (Expands service mesh sidecar tasks & networks)   |
|     ├── mtls                    (Injects native mTLS workload identity hooks)     |
|     ├── expose-check            (Generates exposed health check paths)            |
|     ├── constraints             (Injects implied Vault/Consul/NSD/Signal rules)   |
|     ├── node-pool               (Resolves default node pool)                      |
|     └── secrets                 (Normalizes secret blocks & provider defaults)    |
|                                                                                   |
|  2. External Mutating Webhooks (admissionWebhookMutator):                         |
|     ├── Pre-check: Caller holds "submit-job" ACL in target namespace              |
|     ├── Strips Job.VaultToken & Job.ConsulToken before serialization              |
|     ├── POSTs admission.norca.io/v1 AdmissionReview to mode="mutate" webhooks     |
|     ├── Applies RFC 6902 JSON Patch (verifies ID/Namespace/Region unchanged)      |
|     └── Re-runs jobCanonicalizer if any webhook patched the job                   |
|                                                                                   |
|  3. Built-in Post-Webhook Mutators:                                               |
|     ├── discovery-defaults      (Defaults service discovery on webhook services)  |
|     └── quota-task-defaults     (Injects QuotaSpec.TaskDefaults for CPU/Memory)   |
+-----------------------------------------------------------------------------------+
                                  |
                                  v
+-----------------------------------------------------------------------------------+
| PHASE 2: BUILT-IN STRUCTURAL & QUOTA VALIDATORS (job_endpoint.go:85-100)          |
|                                                                                   |
|  ├── connect, expose-check, vault, namespace-constraint, node-pool                |
|  ├── alloc-affinity, disruption, mtls, scaling, network-policy, secrets           |
|  ├── quota-validation (jobQuotaValidatingHook):                                   |
|  │     ├── Enforces per-task QuotaSpec.TaskMax (CPU, Cores, MemoryMB, MemoryMax)  |
|  │     └── If Enforce == "submit", verifies JobQuotaDemand <= RegionLimit         |
|  └── validate & memory_oversubscription                                           |
+-----------------------------------------------------------------------------------+
                                  |
                                  v
+-----------------------------------------------------------------------------------+
| PHASE 3: ACL & CAPABILITY AUTHORIZATION (job_endpoint.go:158-238)                 |
|                                                                                   |
|  ├── NamespaceCapabilitySubmitJob, CSI/HostVolume mount capabilities              |
|  ├── Cross-namespace allocation affinity ACL check                                |
|  ├── Secret access bindings grant check (caller cannot escalate privileges)       |
|  └── Managed job ownership check (checkManagedJob)                                |
+-----------------------------------------------------------------------------------+
                                  |
                                  v
+-----------------------------------------------------------------------------------+
| PHASE 4: DYNAMIC VALIDATING ADMISSION (job_endpoint_hook_admission.go:359)        |
|                                                                                   |
|  1. Embedded Google CEL Policy Engine (PolicyEngine.Evaluate):                    |
|     ├── Matches policies where AppliesToJob(job.Namespace) == true                |
|     ├── Evaluates compiled ASTs (2s timeout, 1,000,000 cost limit per rule)       |
|     ├── Variables: job, request (operation, namespace, user, secrets)             |
|     └── "warn" -> appends warning; "deny" -> emits JobAdmissionDeniedEvent & 403  |
|                                                                                   |
|  2. External Validating Webhooks (mode="validate"):                               |
|     ├── POSTs AdmissionReview to matching webhooks in configured order            |
|     └── Allowed=false -> emits JobAdmissionDeniedEvent & rejects submission       |
+-----------------------------------------------------------------------------------+
                                  |
                                  v
                      [ RAFT COMMIT: Job Register ]
                                  |
                                  v
+-----------------------------------------------------------------------------------+
| PHASE 5: SCHEDULER & AUTHORITATIVE PLAN APPLIER QUOTA ENFORCEMENT                 |
|                                                                                   |
|  1. Scheduler Worker (QuotaIterator in norca/scheduler/quota.go:30):              |
|     ├── Computes planUsed = snapshotUsed + QuotaPlanDelta(currentPlan)            |
|     ├── Filters candidate nodes where TaskGroupQuotaResources exceeds RegionLimit |
|     └── If filtered: marks EvalEligibility.SetQuotaLimitReached -> BlockedEval    |
|                                                                                   |
|  2. Plan Applier Gate (evaluatePlanQuota in norca/norca/plan_apply_quota.go:17):  |
|     ├── Serialized on leader against optimistic state snapshot                    |
|     ├── Prevents parallel scheduler workers from jointly exceeding quota          |
|     └── If overQuota: rejects plan & returns RefreshIndex to force worker retry   |
|                                                                                   |
|  3. Raft FSM Commit (updateQuotaWithAlloc in state_store_quota.go:408):           |
|     ├── Atomically updates TableQuotaUsage in same WriteTxn as TableAllocs        |
|     ├── Emits QuotaExhausted / QuotaFreed stream events (events_quota.go:61)      |
|     └── Unblocks waiting evaluations (unblockQuotasForPlanResults)                |
+-----------------------------------------------------------------------------------+
```

---

## 3. Admission Policy Engine (`norca/norca/admission/`)

Norca's admission architecture splits responsibility between **mutating controllers** (which normalize, default, or patch incoming objects) and **validating controllers** (which evaluate invariants over the final canonical object).

### 3.1 Mutating vs. Validating Phases & Execution Order

In `NewJobEndpoints` (`norca/norca/job_endpoint.go:66-102`), the server initializes two ordered slices of hooks executed by `admissionControllers` (`norca/norca/job_endpoint_hooks.go:80`):

1. **Mutating Phase (`j.mutators`)**:
   - **Pre-Webhook Built-in Mutators**:
     - `jobCanonicalizer` (`job_endpoint_hooks.go:140`): Invokes `job.Canonicalize()` and populates `job.Priority` from `JobDefaultPriority` if unset.
     - `jobVaultNamespaceDefaulter` (`job_endpoint_hook_vault_namespace.go:14`): Propagates a job-level `VaultNamespace` down to every `task.Vault` stanza that does not explicitly specify its own `Namespace`. Performing this on the server ensures jobs submitted directly via RPC behave identically to jobs converted by the HTTP API.
     - `jobScalingHook`, `jobConnectHook`, `jobMTLSHook`, `jobExposeCheckHook`: Expand scaling policies, Consul Connect sidecars, native mTLS configurations, and exposed health checks.
     - `jobImpliedConstraints` (`job_endpoint_hooks.go:161`): Injects implicit placement constraints (`${attr.vault.version}`, `${attr.consul.version}`, `${attr.nomad.service_discovery}`, required signal constraints, or Norca's `vaultCompatConstraint` when built-in secret store fallback is active).
     - `jobNodePoolMutatingHook`, `jobSecretsHook`: Assign default node pools and normalize secret provider references.
   - **External Mutating Webhooks (`admissionWebhookMutator`, `job_endpoint_hook_admission.go:240`)**:
     - Runs only for `register` and `plan` operations via `MutateWithRequest`.
     - **Pre-Authorization Guard**: Before contacting any external webhook, the mutator resolves the caller's ACL token (`job_endpoint_hook_admission.go:266-272`) and verifies `acl.NamespaceCapabilitySubmitJob`. Unauthenticated or unauthorized callers cannot trigger outbound webhook traffic.
     - If any webhook applies an RFC 6902 JSON Patch (`patched == true`), `jobCanonicalizer` is re-executed (`job_endpoint_hook_admission.go:310-318`) so downstream mutators and validators observe a canonical `structs.Job`.
   - **Post-Webhook Built-in Mutators**:
     - `jobDiscoveryDefaultsHook`: Runs *after* mutating webhooks so that service blocks injected by a webhook receive default service discovery settings.
     - `jobQuotaMutatingHook` (`job_endpoint_hook_quota.go:17`): Runs last in the mutating chain to populate unset task resources (`CPU`, `MemoryMB`, `MemoryMaxMB`) from the namespace quota's `TaskDefaults`.

2. **Validating Phase (`j.validators` & `enforceAdmission`)**:
   - **Built-in Validators (`j.validators`)**: Executed on a defensive deep copy of the mutated job (`origJob.Copy()` in `job_endpoint_hooks.go:120`) to guarantee validators never mutate state. Includes `jobQuotaValidatingHook` (`job_endpoint_hook_quota.go:87`), which checks `TaskMax` and `enforce = "submit"` regional limits.
   - **Dynamic Validating Controllers (`enforceAdmission`, `job_endpoint_hook_admission.go:359`)**: Invoked via `enforceSubmitJob` (`job_endpoint_oss.go:19`) *after* ACL authorization and Consul/Vault token checks pass. Evaluates all matching Raft-stored CEL `AdmissionPolicy` objects first, followed by all matching `mode = "validate"` webhooks.

---

### 3.2 Embedded Google CEL Evaluation Engine (`norca/norca/admission/cel.go`)

The `PolicyEngine` struct (`norca/norca/admission/cel.go:70`) manages the compilation, caching, and sandboxed execution of Common Expression Language rules.

#### 3.2.1 Environment & Type System
`NewPolicyEngine()` (`cel.go:80-100`) constructs a shared `*cel.Env` configured with:
- **Context Variables**: `job` (`cel.DynType`), `request` (`cel.MapType(cel.StringType, cel.DynType)`), `variable` (`cel.DynType`), `oldVariable` (`cel.DynType`), and `release` (`cel.DynType`).
- **Standard Extensions**: `ext.Strings()`, `ext.Sets()`, and `ext.Lists()` (enabling methods such as `.startsWith()`, `.endsWith()`, `.matches()`, `sets.contains()`, `.all()`, `.exists()`, `.filter()`, and `.map()`).
- **Numeric & Temporal Semantics**: `cel.CrossTypeNumericComparisons(true)` allows comparing `int` and `double` values seamlessly; `cel.DefaultUTCTimeZone(true)` pins all timestamp operations to UTC.
- **JSON Number Preservation**: When converting a `structs.Job` or `structs.CustomObject` into a CEL activation map (`JobValue` in `job.go:44`, `ObjectValue` in `object.go:308`), Norca decodes JSON with `UseNumber()` and converts integral values to `int64` (`normalizeNumbers` in `job.go:52-75`) so CEL rules can compare fields like `t.Resources.CPU <= 4000` directly against integer literals.

#### 3.2.2 Compilation Validation, Caching, and Pruning
- **Compile-Time Gate**: When an operator submits a policy via `AdmissionPolicy.UpsertAdmissionPolicies` (`norca/norca/admission_policy_endpoint.go:186`), every rule expression is compiled via `compileRule` (`cel.go:104-118`). The AST output type is verified to be `cel.BoolType` (or `cel.DynType`); expressions returning strings, integers, or maps are rejected with HTTP 400 before touching Raft.
- **Per-Policy ModifyIndex Cache**: `PolicyEngine` maintains an in-memory map `cache map[string]*compiledPolicy` (`cel.go:63-77`). On evaluation, `programs(policy)` checks if `cached.modifyIndex == policy.ModifyIndex`. Recompilation occurs only when a policy is created or updated in Raft.
- **Automatic Cache Pruning**: Whenever `matchingAdmissionPolicies` iterates the state snapshot (`job_endpoint_hook_admission.go:473-491`), it collects the set of active policy names and calls `engine.Prune(names)` (`cel.go:167-175`) to evict deleted policies from memory.

#### 3.2.3 Runtime Sandboxing: Cost Limits, Interrupts, and Timeouts
Because CEL expressions run synchronously on the Raft leader during RPC handling, Norca enforces strict resource limits (`cel.go:18-25`, `113-117`, `230`):

| Parameter | Constant / Value | Source Location | Enforcement Mechanism |
| :--- | :--- | :--- | :--- |
| **Max Rules per Policy** | `maxAdmissionPolicyRules = 64` | `structs/admission.go:69` | Validated on policy upsert |
| **Max Expression Length** | `maxAdmissionRuleExpressionLength = 8192` | `structs/admission.go:73` | Validated on policy upsert (bytes) |
| **Per-Rule Cost Limit** | `celCostLimit = 1_000_000` | `admission/cel.go:20` | `cel.CostLimit(1_000_000)` aborts nested comprehensions exceeding abstract operation budget |
| **Interrupt Check Frequency** | `100` loop iterations | `admission/cel.go:116` | `cel.InterruptCheckFrequency(100)` checks `ctx.Done()` every 100 comprehension steps |
| **Wall-Clock Eval Timeout** | `celEvalTimeout = 2 * time.Second` | `admission/cel.go:24` | `context.WithTimeout(ctx, 2*time.Second)` across all matching policies for a single write |

If any rule fails to compile, exceeds the cost limit, times out, accesses a non-existent map key without `has()`, or evaluates to a non-boolean value, `PolicyEngine.evaluate` (`cel.go:246-264`) records a `Violation`. For a policy with `action = "deny"`, this immediately rejects the write (**fail-closed**).

#### 3.2.4 Exposed CEL Context Variables by Resource Type

| Resource Type | Top-Level CEL Variables | `request` Map Fields | Special Functions / Helpers |
| :--- | :--- | :--- | :--- |
| **Job** (`resource = "job"`) | `job` (API JSON form with `VaultToken` & `ConsulToken` stripped)<br>`variable = null`<br>`oldVariable = null`<br>`release = null` | `request.resource` (`"job"`)<br>`request.operation` (`"register"` \| `"plan"`)<br>`request.namespace`<br>`request.user` (`accessor_id`, `name`, `type`, `roles`, `policies`)<br>`request.secrets` (List of all secret & Vault references) | Standard CEL + `ext.Strings()`, `ext.Sets()`, `ext.Lists()` |
| **Variable** (`resource = "variable"`) | `variable` (`namespace`, `path`, `items`, `create_index`, `modify_index`, `operation`)<br>`oldVariable` (previous decrypted variable or `null`)<br>`job = null`, `release = null` | `request.resource` (`"variable"`)<br>`request.operation` (`"set"` \| `"delete"`)<br>`request.cas` (`bool`)<br>`request.namespace`<br>`request.path`<br>`request.user` | Optional `schema_from = "crd-schema"` Draft 2020-12 JSON Schema validation |
| **Pack Release** (`resource = "release"`) | `release` (`name`, `namespace`, `action`, `pack`, `resources`)<br>`job = null`, `variable = null`, `oldVariable = null` | `request.operation` (`"apply"`)<br>`request.namespace`<br>`request.dryRun` (`bool`)<br>`request.user` (`name`, `type`) | Built-in cosign signature verification via `trusted_keys` |
| **Custom Object** (`ObjectKind.Rules`) | `self` (`kind`, `metadata`, `spec`, `status`)<br>`oldSelf` (stored object on update, `null` on create) | `request.operation` (`"create"` \| `"update"` \| `"status"` \| `"delete"` \| `"migrate"`)<br>`request.namespace`<br>`request.dryRun` (`bool`)<br>`request.user` | `has(oldSelf)` macro (`object.go:82`)<br>`quantity(str\|int\|double)` parser (`object.go:102`) |
| **Object Migration** (`ObjectKind.Migration`) | `self` (`kind`, `metadata`, `spec`, `status`)<br>`fromVersion` (`int`)<br>`toVersion` (`int`) | *(N/A)* | `quantity()`, `m.merge(other)`, `m.omit(["k1", "k2"])` (`migration.go:93`) |

---

### 3.3 External Mutating & Validating HTTP Webhooks (`norca/norca/admission/webhook.go`)

External webhooks allow platform teams to integrate custom policy engines, image scanners, CMDB validators, or sidecar injectors over HTTP/HTTPS.

#### 3.3.1 AdmissionReview Wire Protocol (`admission.norca.io/v1`)
Webhooks receive an HTTP `POST` request with `Content-Type: application/json` containing a `Review` envelope (`norca/norca/admission/webhook.go:29-44`):

```json
{
  "api_version": "admission.norca.io/v1",
  "kind": "AdmissionReview",
  "request": {
    "uid": "a8f5c2d1-4b3e-9a1c-8e2f-1d3c5b7a9e0f",
    "resource": "job",
    "operation": "register",
    "dry_run": false,
    "namespace": "prod",
    "user": {
      "accessor_id": "7d2e1a9b-...",
      "name": "ci-deploy-token",
      "type": "client",
      "roles": ["prod-deployer"],
      "policies": ["prod-write"]
    },
    "job": { "ID": "payments-api", "Namespace": "prod", "...": "..." }
  }
}
```

The webhook server must respond with HTTP `200 OK` (up to `maxResponseSize = 16 MiB`, `webhook.go:33`) and a JSON body echoing the exact `request.uid` (`webhook.go:356-358`):

```json
{
  "api_version": "admission.norca.io/v1",
  "kind": "AdmissionReview",
  "response": {
    "uid": "a8f5c2d1-4b3e-9a1c-8e2f-1d3c5b7a9e0f",
    "allowed": true,
    "warnings": ["Task 'api' uses default CPU allocation"],
    "patch": [
      { "op": "add", "path": "/Meta", "value": { "admitted_by": "sec-webhook" } }
    ]
  }
}
```

#### 3.3.2 RFC 6902 JSON Patch Semantics & Immutability Guards
For `mode = "mutate"` webhooks (and `mode = "default"` custom object webhooks), Norca includes a zero-dependency RFC 6902 JSON Patch engine (`norca/norca/admission/jsonpatch.go:29`):
- **Supported Operations**: `add`, `remove`, `replace`, `move`, `copy`, and `test`, up to `maxPatchOperations = 10000` operations per patch (`jsonpatch.go:17`).
- **Flexible Encoding**: `ReviewResponse.PatchOperations()` (`webhook.go:138-161`) accepts `patch` either as a direct JSON array (`[{"op": "add", ...}]`) or as a base64-encoded JSON array string (matching Kubernetes `AdmissionReview` conventions).
- **Sequential Chaining**: When multiple mutating webhooks match a namespace, they execute in the exact order declared in `norca.hcl`. Each subsequent webhook receives the job incorporating patches applied by earlier webhooks (`job_endpoint_hook_admission.go:278-308`).
- **Security & Immutability Invariants (`applyWebhookPatch`, `job_endpoint_hook_admission.go:324-353`)**:
  1. **Identity & Scope Pinning**: A mutating webhook is strictly forbidden from altering `job.ID`, `job.Namespace`, or `job.Region` (`newJob.ID != job.ID || newJob.Namespace != job.Namespace || newJob.Region != job.Region`). Because ACL pre-authorization is evaluated for the original namespace and region, attempting to move a job across namespaces aborts the mutation.
  2. **Secret Redaction & Restoration**: `EncodeJob` (`admission/job.go:20-30`) clears `job.VaultToken` and `job.ConsulToken` before serializing the job to JSON so external webhooks never see caller tokens. After decoding the patched job, `applyWebhookPatch` restores the original `VaultToken` and `ConsulToken`.
  3. **Custom Object Spec-Only Patching**: For custom objects, `patchObjectSpec` (`object_ecosystem.go:492-531`) verifies via `reflect.DeepEqual` that a defaulting webhook modifies *only* the `/spec` subtree; any modification to `/metadata`, `/kind`, or `/status` is rejected.
  4. **Validating Webhook Patch Ignore**: If a `mode = "validate"` webhook returns a non-empty `patch`, the patch is ignored and surfaced to the caller as a warning (`job_endpoint_hook_admission.go:442-445`).

#### 3.3.3 TLS/mTLS Transport, Redirect Prevention, and Fail-Closed Posture
Each configured webhook initializes a dedicated pooled HTTP client (`newWebhook` in `norca/norca/admission/webhook.go:210-250`):
- **TLS 1.2 Minimum & Custom CAs**: Enforces `tls.VersionTLS12` minimum, optional custom root CA bundle (`ca_file`), optional client certificate keypair for mutual TLS (`cert_file` and `key_file`), and SNI override (`tls_server_name`).
- **SSRF / Redirect Refusal**: `CheckRedirect` unconditionally returns `http.ErrUseLastResponse` (`webhook.go:246-248`). A webhook endpoint can never redirect the Norca server to an internal metadata service or untrusted third-party URL.
- **Single-Shot Execution with Timeout**: `Review()` executes once without automatic retries under `timeout` (default `5s`, `config/admission.go:33`), emitting Prometheus/go-metrics telemetry (`nomad.admission.webhook.latency`, `calls`, `errors`).
- **Fail-Closed by Default (`failure_policy = "fail"`)**:
  - When `failure_policy = "fail"` (the default), any DNS error, TCP reset, TLS handshake error, timeout, non-200 status, malformed JSON, UID mismatch, or invalid JSON patch causes `webhookFailure` (`job_endpoint_hook_admission.go:199-206`) to reject the write immediately.
  - When `failure_policy = "ignore"`, transport or protocol failures are wrapped in `&admissionWarning{}` and returned in the RPC response `Warnings` field while allowing the write to proceed. (Note: an explicit `Allowed: false` response from a reachable webhook *always* rejects the write, even when `failure_policy = "ignore"`.)
- **Self-Deadlock Prevention (`exclude_namespaces`)**: If a webhook server is deployed as a Norca job inside the cluster, operators configure `exclude_namespaces = ["platform-webhooks"]` so that deploying or restarting the webhook job itself is never blocked waiting for the webhook to respond.

---

## 4. Multi-Resource Governance Scope

Norca's admission architecture governs four distinct cluster resource classes:

```
+-----------------------------------------------------------------------------------+
|                        MULTI-RESOURCE ADMISSION MATRIX                            |
+-------------------+-----------------------+-------------------+-------------------+
| Resource Kind     | Mutating / Defaulting | CEL Policy / Rule | Validating        |
|                   | Phase                 | Evaluation        | Webhooks          |
+-------------------+-----------------------+-------------------+-------------------+
| 1. Jobs           | Built-in Mutators +   | PolicyEngine      | Server Webhooks   |
|    ("job")        | Mutating Webhooks +   | (job, request,    | (resources =      |
|                   | Quota TaskDefaults    |  request.secrets) |  ["job"])         |
+-------------------+-----------------------+-------------------+-------------------+
| 2. Variables      | None (Mutating        | PolicyEngine      | Server Webhooks   |
|    ("variable")   | webhooks disallowed   | (variable,        | (opt-in plaintext |
|                   | for Variables)        |  oldVariable) +   |  via include_     |
|                   |                       | crd-schema JSON   |  variable_items)  |
+-------------------+-----------------------+-------------------+-------------------+
| 3. Custom Objects | Schema Defaults +     | ObjectRuleEngine  | Kind-Bound        |
|    ("object")     | CEL Migrations +      | (self, oldSelf,   | Webhooks (require |
|                   | Kind Default Webhooks |  quantity())      |  Admin Approval)  |
+-------------------+-----------------------+-------------------+-------------------+
| 4. Pack Releases  | None                  | PolicyEngine      | Built-in Cosign   |
|    ("release")    |                       | (release,         | TrustedKeys       |
|                   |                       |  release.pack)    | Verification      |
+-------------------+-----------------------+-------------------+-------------------+
```

### 4.1 Jobs & Secret Reference Inspection (`norca/norca/job_endpoint_hook_admission_secrets.go`)
When evaluating CEL policies for a job (`register` or `plan`), Norca populates `request.secrets` via `admissionSecretRefsValue` (`job_endpoint_hook_admission_secrets.go:31-64`). This inspects every secret consumed by the job across four sources:
- Native `secret` stanzas (`source == "secret"`)
- Embedded `template` calls to `nomadSecret` (`source == "template"`)
- Task `vault` stanzas (`source == "vault"`)
- Embedded `template` calls to Vault secrets (`source == "vault_template"`)

Each entry in `request.secrets` exposes `{group, task, source, name, provider, default_provider, effective_provider, path, version, file_format, env, optional}`, allowing CEL rules to enforce secret path prefixes, mandate specific secret providers (e.g. `s.effective_provider == "bao"`), or forbid injecting secrets into environment variables (`size(s.env) == 0`).

### 4.2 Encrypted Variables & The `crd-schema` Pattern (`norca/norca/variables_endpoint_admission.go`)
Variables admission (`Variables.enforceAdmission` in `variables_endpoint_admission.go:103`) runs on the leader after ACL authorization (`variables:write` or `variables:destroy`) and before encryption and Raft persistence:
1. **Path & Namespace Filtering**: Policies and webhooks opt into Variables governance by specifying `resources = ["variable"]` and optional glob patterns in `variable_paths = ["crd/*", "app/prod/*"]` (`structs/admission.go:301-308`).
2. **Lazy Decryption of `oldVariable`**: If a CEL policy or a webhook with `include_variable_items = true` inspects an update or deletion, `variableAdmission.old()` (`variables_endpoint_admission.go:62-73`) lazily fetches and decrypts the existing variable from the state snapshot once.
3. **Privacy-by-Default for Webhooks**: Unless a webhook explicitly configures `include_variable_items = true` in `norca.hcl`, `ReviewVariable` (`admission/variable.go:81-98`) transmits *only* metadata (`namespace`, `path`, `create_index`, `modify_index`) and omits `items`. Furthermore, `VariableAdmissionDeniedEvent` (`structs/admission.go:443`) never includes variable items, and schema validation failure events sanitize the event message to `"schema validation failed"` (`variables_endpoint_admission.go:251`).
4. **JSON Schema Validation (`schema_from = "crd-schema"`)**:
   - When an `AdmissionPolicy` sets `schema_from = "crd-schema"` (`admission/variable.go:19-44`, `144-201`), any `set` operation on a variable at `crd/<group>/<kind>/<name>` automatically looks up the corresponding schema variable at `crd-schema/<group>/<kind>` in the same namespace.
   - Furthermore, writing to `crd-schema/<group>/<kind>` itself validates that its `schema` item compiles as a valid JSON Schema Draft 2020-12 document (`variables_endpoint_admission.go:267-272`).
   - Compiled schemas are cached in `SchemaCache` (`admission/variable.go:231`, up to `maxCachedSchemas = 512`) keyed by `namespace + "\x00" + path` and `ModifyIndex`.
   - External `$ref` resolution is blocked via `noLoader{}` (`admission/variable.go:135-139`), preventing Server-Side Request Forgery (SSRF) or local file disclosure during schema compilation.
   - If the `crd-schema` variable sets `encoding = "json"`, each item value in `crd/<group>/<kind>/<name>` is parsed as JSON before validation; otherwise (`encoding = "string"`, default), items are validated as a JSON object of string values.

### 4.3 Custom Objects & Operator Framework (`norca/norca/admission/object.go`, `migration.go`)
Norca's native Operator Framework (`ObjectKind` and `CustomObject` in `norca/norca/object_endpoint.go` and `object_ecosystem.go`) integrates three dedicated admission mechanisms:
1. **CEL Transition & Validation Rules (`ObjectRuleEngine`, `admission/object.go:46`)**:
   - Each `ObjectKind` can define CEL `rules` evaluated on `create`, `update`, and `status` writes (`object_endpoint.go:860-879`).
   - Rules access `self` (`{"kind", "metadata", "spec", "status"}`), `oldSelf` (the stored object on update, or `null` on create), and `request`.
   - **Custom `has(oldSelf)` Macro (`object.go:82-98`)**: Extends CEL's standard `has()` macro so `has(oldSelf)` rewrites AST-wise to `oldSelf != null`, enabling clean immutability rules such as `!has(oldSelf) || self.spec.storageClass == oldSelf.spec.storageClass`.
   - **Built-in `quantity()` Function (`object.go:100-168`)**: Parses Kubernetes/SI/binary quantity strings (`"100GiB"`, `"1.5Gi"`, `"500m"`, `"2k"`) into `double` values so rules can enforce storage or resource bounds (e.g., `quantity(self.spec.disk) <= quantity("500GiB")`).
2. **CEL Schema Migrations (`MigrationEngine`, `admission/migration.go:47`)**:
   - When an `ObjectKind` raises its `version`, its `migration` block defines CEL expressions for `spec` and/or `status` that transform stored objects from `fromVersion` to `toVersion`.
   - Provides custom CEL map methods `m.merge(other)` and `m.omit(["k1", "k2"])` (`migration.go:93-121`).
   - During `UpsertObjectKind`, `checkStoredObjects` (`object_ecosystem.go:643-713`) dry-runs the migration and new schema against *every existing object in the state store*, rejecting the kind update with HTTP 409 (`migrationReportError`) if any stored object would become invalid.
3. **Type-Bound Webhooks & Mandatory Administrator Approval (`object_ecosystem.go:399-404`, `578-601`, `749-805`)**:
   - An `ObjectKind` may bundle `webhook` definitions (`mode = "default"` or `mode = "validate"`).
   - Because an `ObjectKind` can be installed by a non-management token with `object-kind:write`, Norca prevents webhook SSRF/exfiltration by requiring **explicit management token approval** (`ObjectKindWebhookApproval`, keyed by the SHA-256 `Digest()` of the webhook's URL, mode, CA cert, server name, operations, timeout, and failure policy).
   - Until an administrator runs `norca object-kind webhook approve <group/Kind> <webhook>` with a management token, `w.Approved()` is `false` and calls to the webhook fail closed (unless `failure_policy = "ignore"`).

### 4.4 Pack Releases & Cryptographic Signature Verification (`norca/norca/release_admission.go`)
When a pack release is installed, upgraded, or rolled back (`releaseManager.admitRelease` in `norca/norca/release_admission.go:26`), policies matching `resources = ["release"]` enforce supply-chain security in two stages:
1. **Cosign-Compatible Signature Verification (`norca/norca/admission/signature.go`)**:
   - Policies may specify up to 32 PEM-encoded PKIX public keys in `trusted_keys` (`maxAdmissionTrustedKeys = 32`, `structs/admission.go:53`). Supported algorithms are **ECDSA**, **Ed25519**, and **RSA (>= 2048 bits)** (`signature.go:51-59`).
   - Each key is assigned a deterministic key ID: `sha256:<hex(SHA256(PKIX_DER))>` (`signature.go:60-61`).
   - `VerifyPackSignatures` (`signature.go:74-99`) verifies the base64 signature over the cosign "simple signing" JSON payload and confirms `critical.image.docker-manifest-digest == pack.Digest`.
   - If verified, `pack.TrustedSigner` is set to the matching key ID (`sha256:...`). If no trusted key matches (or the pack lacks an OCI digest), the policy triggers a violation (`release_admission.go:70-80`).
2. **Release CEL Rules (`EvaluateRelease`, `release_admission.go:83-110`)**:
   - Evaluates CEL rules against `release` (`name`, `namespace`, `action`, `pack: {name, version, registry, path, digest, signer, signed, trustedSigner}`, and `resources: [{type, namespace, name}]`).

---

## 5. Resource Quota Architecture (`norca/norca/quota_endpoint.go`, `plan_apply_quota.go`)

Norca provides a complete, open-source implementation of multi-region **Resource Quotas** and per-task **LimitRanges**, enforcing finite capacity across namespaces.

### 5.1 Quota Specification Data Model & Multi-Region Replication

A `QuotaSpec` (`norca/norca/structs/quota.go:73-104`) is a global cluster object bound to one or more namespaces via `Namespace.Quota`:

```
+-----------------------------------------------------------------------------------+
|                              QuotaSpec ("prod-quota")                             |
|  Enforce: "schedule" (default) | "submit"                                         |
|  TaskDefaults: { CPU: 100, MemoryMB: 256, MemoryMaxMB: 512 }                      |
|  TaskMax:      { CPU: 4000, Cores: 4, MemoryMB: 8192, MemoryMaxMB: 16384 }        |
|  Hash:         BLAKE2b-256(Name, Description, Enforce, Limits, Defaults, Max)     |
+-----------------------------------------+-----------------------------------------+
                                          |
                  +-----------------------+-----------------------+
                  |                                               |
                  v                                               v
+-----------------------------------+           +-----------------------------------+
|   QuotaLimit (Region: "us-east")  |           |   QuotaLimit (Region: "eu-west")  |
|  RegionLimit:                     |           |  RegionLimit:                     |
|    CPU:         50000 (MHz)       |           |    CPU:         25000 (MHz)       |
|    MemoryMB:    65536 (MiB)       |           |    MemoryMB:    32768 (MiB)       |
|    MemoryMaxMB: 98304 (MiB)       |           |    MemoryMaxMB: 32768 (MiB)       |
|  VariablesLimit: 500  (MiB)       |           |  VariablesLimit: 100  (MiB)       |
|  Allocations:    200  (count)     |           |  Allocations:    100  (count)     |
|  Hash: BLAKE2b-256 -> HashKey()   |           |  Hash: BLAKE2b-256 -> HashKey()   |
+-----------------------------------+           +-----------------------------------+
```

#### Limit Semantics (`structs/quota.go:107-131`, `465-487`):
- **`limit > 0`**: Finite ceiling enforced in that dimension.
- **`limit == 0` (or `nil`)**: Unlimited capacity in that dimension.
- **`limit < 0`**: Dimension is **completely disallowed** (`value > 0` is rejected; remaining capacity is `0`).
- **Multi-Region Federation (`norca/norca/leader_quota.go:19-133`)**:
  - `QuotaSpec` writes (`Quota.UpsertQuotaSpecs`, `Quota.DeleteQuotaSpecs`) are automatically forwarded to the `AuthoritativeRegion` (`quota_endpoint.go:34`, `83`).
  - Non-authoritative region leaders run `replicateQuotaSpecs` (`leader_quota.go:19`), which performs blocking queries against `Quota.ListQuotaSpecs` in the authoritative region and computes a two-way diff using each spec's **BLAKE2b-256** `Hash` (`diffQuotaSpecs`, `leader_quota.go:104`).
  - **Local Usage Autonomy**: `QuotaUsage` is *never* replicated across regions. Each region tracks the usage of its own local allocations against the `QuotaLimit` matching its local region name (`state_store_quota.go:350`).

---

### 5.2 Mathematical Formulation of Quota Accounting

#### 5.2.1 Task Lifecycle Resource Aggregation
Within a single allocation, tasks do not all execute simultaneously: ephemeral `prestart` hooks finish before `main` tasks start, and `poststop` hooks run only after `main` tasks exit, whereas `prestart` sidecar tasks (`Sidecar = true`) run alongside `main` tasks for the lifetime of the allocation.

`aggregateQuotaTaskUsage` (`norca/norca/structs/quota.go:658-679`) computes an allocation's resource footprint $U_{\text{alloc}}$ across task groups $T_{\text{sidecar}}, T_{\text{ephemeral}}, T_{\text{main}}, T_{\text{poststop}}$ identically to node fit calculation:

$$U_{\text{alloc}}^{(d)} = \sum_{t \in T_{\text{sidecar}}} u_t^{(d)} + \max\!\left( \sum_{t \in T_{\text{ephemeral}}} u_t^{(d)},\; \sum_{t \in T_{\text{main}}} u_t^{(d)},\; \sum_{t \in T_{\text{poststop}}} u_t^{(d)} \right) \quad \forall d \in \{\text{CPU}, \text{MemoryMB}, \text{MemoryMaxMB}\}$$

For any task $t$ that does not specify `MemoryMaxMB` (or sets `MemoryMaxMB == 0`), its effective maximum memory defaults to its reserved memory (`memMax = tr.Memory.MemoryMB`, `structs/quota.go:696-699`). Every non-terminal allocation (`!alloc.TerminalStatus()`) contributes $U_{\text{alloc}}^{(\text{Allocations})} = 1$, while terminal allocations contribute $\vec{0}$ (`AllocQuotaResources`, `structs/quota.go:684-687`).

#### 5.2.2 Reserved CPU Cores to MHz Conversion
While quotas specify CPU limits in MHz (`RegionLimit.CPU`), tasks may request dedicated CPU cores (`Resources.Cores`). During scheduling on a candidate node $n$, `TaskGroupQuotaResources` (`structs/quota.go:734-750`) and `sharesPerCore` (`scheduler/quota.go:208-213`) convert reserved cores into MHz using the node's hardware ratio:

$$u_t^{(\text{CPU})}(n) = \begin{cases} \text{Cores}_t \times \left\lfloor \dfrac{\text{NodeTotalCpuShares}(n)}{\text{NodeTotalCpuCores}(n)} \right\rfloor & \text{if } \text{Cores}_t > 0 \\[8pt] \text{CPU}_t & \text{otherwise} \end{cases}$$

#### 5.2.3 Plan Delta Calculation & Over-Commitment Recovery
When evaluating a scheduler `Plan` $P$, `QuotaPlanDelta` (`structs/quota.go:800-846`) determines the net change $\Delta U(P)$ across all namespaces $NS(q)$ bound to quota $q$. It constructs the final state of every touched allocation across `plan.NodeUpdate` (stopped/updated allocs), `plan.NodePreemptions` (preempted allocs), and `plan.NodeAllocation` (newly placed or in-place updated allocs), where later maps take precedence:

$$\Delta U(P) = \sum_{\substack{a \in \text{Final}(P) \\ \text{ns}(a) \in NS(q)}} \Big( U_{\text{alloc}}(a_{\text{new}}) - U_{\text{alloc}}(a_{\text{existing}}) \Big)$$

`QuotaLimit.CheckAdditional(used, delta)` (`structs/quota.go:540-554`) enforces a critical liveness property: **a dimension $d$ is reported as violated if and only if the plan strictly increases usage in $d$ AND the new total exceeds the limit**:

$$\text{Violated}(d) \iff \left( \Delta U^{(d)} > 0 \right) \;\land\; \text{quotaLimitExceeded}\!\left(L^{(d)},\; U_{\text{used}}^{(d)} + \Delta U^{(d)}\right)$$

Because dimensions with $\Delta U^{(d)} \le 0$ are never flagged, **plans that stop allocations, scale down, or perform equal-sized replacements in an already over-committed quota (e.g., after an operator lowers a quota limit) are always permitted to proceed**.

#### 5.2.4 Autoscaler Ceiling Calculation (`MaxAllocs`)
Before scaling a task group up, the autoscaler queries `StateStore.QuotaRemainingForNamespace` (`state_store_quota.go:188`) and invokes `QuotaRemaining.MaxAllocs(per)` (`structs/quota.go:580-599`) to cap scale-up recommendations at the remaining regional quota capacity:

$$\text{MaxAllocs}(R, U_{\text{tg}}) = \min_{\substack{d \in \{\text{CPU}, \text{Mem}, \text{MemMax}, \text{Allocs}\} \\ L^{(d)} \neq 0,\; U_{\text{tg}}^{(d)} > 0}} \left\lfloor \frac{\max\!\big(0,\; L^{(d)} - U_{\text{used}}^{(d)}\big)}{U_{\text{tg}}^{(d)}} \right\rfloor$$

#### 5.2.5 Encrypted Variables Quota Accounting
Variables storage usage is tracked per namespace in `TableVariablesQuotas` (in bytes) and aggregated across all namespaces bound to a quota via `quotaVariablesSizeTxn` (`state_store_quota.go:458-481`). During any `Variables` write that increases storage size ($\Delta B > 0$), `enforceVariablesQuota` (`state_store_quota.go:486-519`) verifies:

$$\sum_{ns \in NS(q)} \text{SizeBytes}(ns) + \Delta B \le \text{VariablesLimit}_{\text{MiB}} \times 1048576$$

When queried via `Quota.GetQuotaUsage`, `QuotaUsageWithVariables` (`state_store_quota.go:139-156`) rounds total byte usage up to the nearest MiB:
$$\text{UsedVariables}_{\text{MiB}} = \left\lceil \frac{\sum_{ns \in NS(q)} \text{SizeBytes}(ns)}{1048576} \right\rceil$$

---

### 5.3 Three-Tier Quota Enforcement Lifecycle

```
+-----------------------------------------------------------------------------------+
| TIER 1: JOB SUBMISSION ADMISSION (norca/norca/job_endpoint_hook_quota.go)         |
|                                                                                   |
|  ├── jobQuotaMutatingHook ("quota-task-defaults"):                                |
|  │     If task resource is 0 or equals Nomad's built-in default (100 MHz /        |
|  │     300 MiB), overwrites with QuotaSpec.TaskDefaults.                          |
|  └── jobQuotaValidatingHook ("quota-validation"):                                 |
|        1. Rejects any task exceeding QuotaSpec.TaskMax (CPU, Cores, Mem, MemMax). |
|        2. If Enforce == "submit" ("hard submission mode"):                        |
|           Computes total JobQuotaDemand = sum(tg.Count * TaskGroupQuotaResources) |
|           and rejects the job immediately if demand > RegionLimit.                |
+-----------------------------------------------------------------------------------+
                                         |
                                         v
+-----------------------------------------------------------------------------------+
| TIER 2: SCHEDULER FEASIBILITY FILTERING (norca/scheduler/quota.go)                |
|                                                                                   |
|  ├── QuotaIterator wraps the node FeasibleIterator pipeline.                      |
|  ├── SetJob(job): Loads QuotaSpec, RegionLimit, and QuotaUsage from snapshot.     |
|  ├── SetTaskGroup(tg):                                                            |
|  │     Computes planUsed = iter.used + QuotaPlanDelta(ctx.Plan()).                |
|  │     If task group does not reserve CPU cores, checks CheckAdditional upfront.  |
|  └── Next():                                                                      |
|        If task group reserves cores, converts cores -> MHz per candidate node.    |
|        When quota is exhausted:                                                   |
|          1. Records exhausted dimensions in AllocMetrics.ExhaustQuota(dims).      |
|          2. Calls ctx.Eligibility().SetQuotaLimitReached(quotaName).              |
|          3. Scheduler creates a BlockedEvaluation waiting on quotaName.           |
+-----------------------------------------------------------------------------------+
                                         |
                                         v
+-----------------------------------------------------------------------------------+
| TIER 3: AUTHORITATIVE PLAN APPLIER & FSM COMMIT (plan_apply_quota.go, fsm_quota)  |
|                                                                                   |
|  ├── evaluatePlanQuota (norca/norca/plan_apply_quota.go:17):                      |
|  │     Executes single-threaded in plan_apply.go:517 against the leader's         |
|  │     optimistic state snapshot (which includes all previously committed plans). |
|  │     If overQuota == true:                                                      |
|  │       Rejects the entire plan and returns PlanResult{RefreshIndex: index},     |
|  │       forcing the scheduler worker to refresh its state and re-evaluate.       |
|  ├── Preemption Interaction:                                                      |
|  │     If a plan preempts allocations in the same namespace/quota,                |
|  │     QuotaPlanDelta subtracts the preempted allocations' resources from the     |
|  │     plan's net delta, allowing higher-priority jobs to displace lower-priority |
|  │     allocations within a full quota!                                           |
|  └── FSM State Commit & Unblocking (fsm_quota.go & state_store_quota.go):         |
|        - updateQuotaWithAlloc updates TableQuotaUsage in the same MemDB WriteTxn. |
|        - unblockQuotasForPlanResults (fsm_quota.go:86) and applyQuotaSpecUpsert   |
|          (fsm_quota.go:41) call blockedEvals.UnblockQuota(quota, index) to wake   |
|          blocked evaluations as soon as allocations stop or limits are raised.    |
+-----------------------------------------------------------------------------------+
```

### 5.4 MemDB State Tables, Raft MessageTypes, and Event Stream

| Component | Identifier / Constant | Value / Schema | Source File |
| :--- | :--- | :--- | :--- |
| **MemDB Table (Admission)** | `TableAdmissionPolicies` | `"admission_policies"` (Unique `id` index on `Name`) | `state/state_store_admission.go:14` |
| **MemDB Table (Quota Specs)** | `TableQuotaSpecs` | `"quota_specs"` (Unique `id` index on `Name`) | `state/state_store_quota.go:14` |
| **MemDB Table (Quota Usage)** | `TableQuotaUsage` | `"quota_usage"` (Unique `id` index on `Name`) | `state/state_store_quota.go:15` |
| **MemDB Index (Namespaces)** | `indexQuota` | `"quota"` index on `TableNamespaces` | `state/state_store_quota.go:18` |
| **Raft MsgType (Quota Upsert)**| `QuotaSpecUpsertRequestType` | `MessageType = 66` (`QuotaSpecSnapshot = 66`) | `structs/structs.go:136`, `fsm_quota.go:22` |
| **Raft MsgType (Quota Delete)**| `QuotaSpecDeleteRequestType` | `MessageType = 67` (`QuotaUsageSnapshot = 67`) | `structs/structs.go:137`, `fsm_quota.go:23` |
| **Raft MsgType (Policy Upsert)**| `AdmissionPolicyUpsertRequestType`| `MessageType = 70` (`AdmissionPolicySnapshot = 70`) | `structs/structs.go:142`, `fsm.go:73` |
| **Raft MsgType (Policy Delete)**| `AdmissionPolicyDeleteRequestType`| `MessageType = 71` | `structs/structs.go:143` |
| **Event Topic (Admission)** | `TopicAdmissionPolicy` | `"AdmissionPolicy"`: `AdmissionPolicyUpserted`, `AdmissionPolicyDeleted`, `JobAdmissionDenied`, `VariableAdmissionDenied` | `structs/admission.go:79-84` |
| **Event Topic (Quota)** | `TopicQuota` | `"Quota"`: `QuotaSpecUpserted`, `QuotaSpecDeleted`, `QuotaExhausted`, `QuotaFreed` | `structs/quota.go:41-53` |

---

## 6. HCL & JSON Configuration Reference

### 6.1 Server Webhook Configuration (`norca.hcl`)

External mutating and validating webhooks are configured in the server's `admission` stanza (`norca/norca/structs/config/admission.go:45-107`):

```hcl
admission {
  # 1. Mutating Webhook: Injects standard labels/metadata or sidecars into jobs
  webhook "platform-mutator" {
    url                = "https://webhooks.internal.corp:8443/mutate"
    mode               = "mutate"
    timeout            = "3s"
    failure_policy     = "fail" # Fail-closed: reject job if webhook is unreachable
    namespaces         = ["prod-*", "staging-*"]
    exclude_namespaces = ["platform-webhooks", "kube-system"]
    resources          = ["job"]

    ca_file         = "/etc/norca/tls/webhook-ca.pem"
    cert_file       = "/etc/norca/tls/server-client.pem"
    key_file        = "/etc/norca/tls/server-client-key.pem"
    tls_server_name = "webhooks.internal.corp"
  }

  # 2. Validating Webhook: Audits jobs and Variables writes
  webhook "security-validator" {
    url                    = "https://webhooks.internal.corp:8443/validate"
    mode                   = "validate"
    timeout                = "5s"
    failure_policy         = "fail"
    resources              = ["job", "variable"]
    variable_paths         = ["prod/*", "crd/*"]
    include_variable_items = false # Only send variable path/metadata, never plaintext items

    ca_file = "/etc/norca/tls/webhook-ca.pem"
  }
}
```

### 6.2 CEL Admission Policy Examples (`norca admission-policy apply`)

#### Example A: Production Job Hardening & Secret Governance Policy
```hcl
name        = "prod-workload-hardening"
description = "Enforce non-privileged containers, approved registries, and secret provider rules in prod"
action      = "deny" # or "warn"

match {
  namespaces = ["prod", "prod-*"]
  resources  = ["job"]
}

rule {
  expression = "!job.TaskGroups.exists(g, g.Tasks.exists(t, t.Driver == 'docker' && has(t.Config.privileged) && t.Config.privileged == true))"
  message    = "Privileged Docker containers are forbidden in production namespaces."
}

rule {
  expression = "job.TaskGroups.all(g, g.Tasks.all(t, t.Driver != 'docker' || t.Config.image.startsWith('registry.internal.corp/')))"
  message    = "All Docker images in production must originate from registry.internal.corp/."
}

rule {
  expression = "request.secrets.all(s, s.effective_provider != '' && s.path.startsWith('prod/') && size(s.env) == 0)"
  message    = "Production secrets must use an explicit provider, reside under prod/, and be mounted as files rather than environment variables."
}
```

#### Example B: Encrypted Variables & `crd-schema` Validation Policy
```hcl
name        = "crd-variable-validation"
description = "Validate CRD Variables against Draft 2020-12 JSON Schemas stored in crd-schema/*"
action      = "deny"
schema_from = "crd-schema"

match {
  namespaces     = ["*"]
  resources      = ["variable"]
  variable_paths = ["crd/*", "crd-schema/*"]
}

rule {
  expression = "request.operation != 'delete' || 'platform-admin' in request.user.roles"
  message    = "Only tokens with the platform-admin role may delete CRD variables."
}
```

#### Example C: Pack Release Supply-Chain Signature & Registry Policy
```hcl
name        = "signed-pack-releases"
description = "Require cosign cryptographic signatures and approved OCI registries for pack releases"
action      = "deny"

match {
  namespaces = ["prod*"]
  resources  = ["release"]
}

trusted_keys = [
  <<-EOT
  -----BEGIN PUBLIC KEY-----
  MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE8z2m...
  -----END PUBLIC KEY-----
  EOT
]

rule {
  expression = "release.pack.signed && release.pack.registry.startsWith('oci://registry.internal.corp/')"
  message    = "Releases in production must use signed packs from oci://registry.internal.corp/."
}
```

### 6.3 Resource Quota & Task LimitRange Specification (`norca quota apply`)

```hcl
name        = "prod-team-quota"
description = "Aggregate regional quota and per-task LimitRange for production namespaces"

# "schedule" (default) places allocations up to the limit and queues the rest as blocked evals.
# "submit" additionally rejects jobs at registration if their total demand exceeds the limit.
enforce = "schedule"

# Regional aggregate limits across all namespaces bound to "prod-team-quota"
limit {
  region = "us-east-1"

  region_limit {
    cpu        = 50000 # 50,000 MHz total CPU (includes reserved cores converted to MHz)
    memory     = 65536 # 64 GiB total reserved memory
    memory_max = 98304 # 96 GiB total memory_max ceiling
  }

  variables_limit = 512 # 512 MiB total encrypted Variables storage
  allocations     = 250 # Maximum 250 non-terminal allocations
}

limit {
  region = "eu-central-1"

  region_limit {
    cpu        = 25000
    memory     = 32768
    memory_max = 49152
  }

  variables_limit = 256
  allocations     = 125
}

# Injected into tasks that omit CPU/memory (or leave them at Nomad's built-in defaults)
task_defaults {
  cpu        = 200
  memory     = 256
  memory_max = 512
}

# Hard ceiling for any single task submitted to a namespace using this quota
task_max {
  cpu        = 8000
  cores      = 4
  memory     = 16384
  memory_max = 32768
}
```

---

## 7. CLI Command Cheat-Sheet

### 7.1 Admission Policy Commands (`norca admission-policy`)

| Command | Action | Required ACL | Key Flags |
| :--- | :--- | :--- | :--- |
| `norca admission-policy apply <file\|->` | Compile and upsert a CEL admission policy from HCL or JSON | `admission:write` | `-json` |
| `norca admission-policy list` | List all admission policies in the region | `admission:read` | `-prefix`, `-filter`, `-per-page`, `-page-token`, `-json`, `-t` |
| `norca admission-policy inspect <name>` | Display full rules, match selectors, and Raft indexes of a policy | `admission:read` | `-json`, `-t` |
| `norca admission-policy delete <name>` | Delete an admission policy from the region | `admission:write` | *(standard client flags)* |
| `norca object-kind webhook approve <group/Kind> <webhook>` | Approve (or `-revoke`) a type-bound webhook on a Custom Object Kind | `management` token | `-revoke` |

### 7.2 Resource Quota Commands (`norca quota`)

| Command | Action | Required ACL | Key Flags |
| :--- | :--- | :--- | :--- |
| `norca quota init [filename]` | Generate an example `spec.hcl` or `spec.json` quota specification | *(None / Local)* | `-json` |
| `norca quota apply <file\|->` | Create or update a quota specification (forwarded to authoritative region) | `quota:write` | `-json` |
| `norca quota list` | List all registered quota specifications | `quota:read` | `-json`, `-t` |
| `norca quota status <name>` | View live per-region usage vs. limits (`CPU`, `Memory`, `Memory Max`, `Variables`, `Allocations`), `Enforce`, `Task Defaults`, and `Task Max` | `quota:read` | `-json`, `-t` |
| `norca quota inspect <name>` | Dump raw JSON of the `QuotaSpec` alongside per-region `QuotaUsage` maps | `quota:read` | `-json`, `-t` |
| `norca quota delete <name>` | Delete an unused quota specification (fails with 400 if attached to any namespace) | `quota:write` | *(standard client flags)* |

---

## 8. Summary for Platform Engineers and Security Officers

1. **Webhooks Cannot Be Registered via API**: Cluster-wide admission webhooks are intentionally restricted to `norca.hcl` server configuration (`config/admission.go:42-44`), while custom object webhooks embedded in `ObjectKind` definitions require explicit `management` token approval (`object_ecosystem.go:402`, `760`) tied to a cryptographic SHA-256 configuration digest.
2. **Secrets Are Stripped Before Webhook Transmission**: Job-level `VaultToken` and `ConsulToken` fields are scrubbed prior to JSON encoding (`admission/job.go:22-23`), and encrypted Variables items are omitted from webhook payloads unless `include_variable_items = true` is explicitly enabled on the server (`admission/variable.go:90`).
3. **Always Exclude the Webhook's Own Namespace**: When hosting mutating or validating webhook workloads inside Norca itself, always set `exclude_namespaces` on the `webhook` stanza to prevent circular admission deadlocks during cluster recovery.
4. **Monitor Admission & Quota Event Streams**: Subscribe to the `AdmissionPolicy` event stream topic (`JobAdmissionDenied`, `VariableAdmissionDenied`) for real-time SIEM security auditing, and the `Quota` topic (`QuotaExhausted`, `QuotaFreed`) to alert on capacity bottlenecks before workloads stall in blocked evaluations.
