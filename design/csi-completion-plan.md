<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# CSI completion plan

**Status:** Draft · 2026-11-11 · decisions 1–3 taken by the user · phases 1–5 built and merged (norca #19–#22, #24; doc #28–#31) · clean room (see
[Clean room](#clean-room))

Norca speaks CSI spec v1.7.0 (`github.com/container-storage-interface/spec v1.7.0`). The volume
life cycle (create, delete, register, claim, publish, stage, expand), snapshots, the list of external
volumes and topology work. Several parts of the spec are missing or only half wired: the plugin
client has no `NodeGetVolumeStats`, `GetCapacity` or `ControllerGetVolume`; a read-only
`ControllerPublishVolume` was sent to plugins without `PUBLISH_READONLY`, against the spec; the newer access modes and the `VOLUME_MOUNT_GROUP` node capability are
not known. (Corrected 2026-11-11: `GetPluginInfo` is called, as `PluginInfo()`, and
`plugin status` shows the vendor and version; most capability checks exist through
`HasControllerCapability`.)

## Clean room

Some of these features exist in HashiCorp's BSL releases of Nomad. The source of this plan is the
**CSI specification** (`container-storage-interface/spec`, Apache-2.0: `spec.md` and `csi.proto`) and
the Norca tree (the MPL v1.6.5 base and Norca's own code). No BSL release of Nomad, its code, its
documentation or its changelog entries for these features is read. Commits say "clean room", so the
`bugfix` set of the similarity audit compares their lines; the `full` set compares the new files
with the newest release.

## Decisions (user, 2026-11-11)

1. **Volume statistics:** on wherever a node plugin advertises `GET_VOLUME_STATS`, every **1 minute**
   by default; the interval is configurable, `0` turns them off.
2. **Volume health:** **reported and published as events**; no automatic reaction (no restart or
   reschedule of allocations).
3. **Capacity:** a periodic capacity metric is **needed**, with a configurable interval.

## Where each part goes

| # | Part | Configuration | Server / scheduler | Client | Operator (CLI / API) | Observability |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Volume statistics (`NodeGetVolumeStats`) | `csi_plugin { stats_interval = "1m" }` on the node plugin task; default `1m` when the plugin has `GET_VOLUME_STATS`; `0` = off | Records the reports; the volume read returns the usage per node | Asks the node plugin for every volume it has published, at the interval; sends the result with the usage report of the node | `norca volume status <id>`: used and available bytes and inodes per node; the same in `GET /v1/volume/csi/<id>` | Metrics store: `norca.volume.used_bytes`, `norca.volume.available_bytes`, `norca.volume.inodes_used`, `norca.volume.inodes_free` (labels: `volume`, `ns`, `node`) |
| 2 | Volume health (`VOLUME_CONDITION`) | None | Keeps a `Condition` per volume and node (abnormal, message, since); publishes a change | The node side comes with the statistics of 1 (the `volume_condition` of the `NodeGetVolumeStats` reply); the controller side from `ListVolumes` / `ControllerGetVolume` (4) | `norca volume status` shows it; `-abnormal` filters | New event topic **`CSIVolume`**: `VolumeAbnormal`, `VolumeRecovered`; gauge `norca.volume.abnormal` |
| 3 | Capacity (`GetCapacity`) | `csi_plugin { capacity_interval = "5m" }` on the controller plugin task; `0` = off | The leader asks the controller plugin at the interval, through the client that runs it (as `ClientCSI.Controller*` today): once without topology, and once per topology segment that the node plugins of the plugin report; checks capacity before `volume create` | Calls `GetCapacity` for the server (`ClientCSI.ControllerGetCapacity`) | `norca plugin capacity <plugin> [-topology k=v ...] [-param k=v ...]`; `GET /v1/plugin/csi/<id>/capacity` | Metrics store: `norca.csi.capacity_bytes` (labels: `plugin`, `segment`), the largest volume `norca.csi.max_volume_bytes` when the plugin reports it |
| 4 | Live volume state (`ControllerGetVolume`) | None | The volume watcher compares the nodes the plugin reports the volume published to with the claims in Raft and reports a difference | `ClientCSI.ControllerGetVolume` | `norca volume status <id> -live` | `CSIVolume` event `VolumeDrift` |
| 5 | Plugin info (`GetPluginInfo`) | None | The plugin record keeps the vendor name and version | Fingerprint | `norca plugin status` shows them | None |
| 6a | Access modes `single-node-single-writer`, `single-node-multi-writer` | `capability { access_mode = ... }` of the volume specification and the `volume` block of the job | Claims: one writing allocation on one node; several writing allocations on one node; the scheduler filters | Passes the mode to the plugin | `norca volume status` shows it | None |
| 6b | Mount group (`VOLUME_MOUNT_GROUP`) | `mount_options { mount_group = "1000" }` in the `volume` block of the job | Refuses it for a node plugin without the capability | Passes it in `NodeStage` / `NodePublish` | None | None |
| 7 | Checks before the plugin call | None | `create` (with `clone_id` or `snapshot_id`), `snapshot create`, expansion and read-only publish are refused at once, with a clear error, when the plugin lacks the capability | None | Better errors | None |
| 8 | CSI spec 1.9+ (group snapshots, `ControllerModifyVolume`, snapshot metadata) | Not planned now; a separate evaluation | | | | |

The defaults keep today's behaviour except the statistics (on, every minute, decision 1). The web UI
is left as it is until its rework; the parts are added to `ui-design-review.md` when they are built.

## Details

### 1. Volume statistics

- The `csi_plugin` block gets `stats_interval` (duration, default `1m`, `0` off). It applies to node
  and monolith plugins; a plugin without `GET_VOLUME_STATS` is not asked.
- The client keeps, per published volume, the target path of each allocation (what it gave
  `NodePublishVolume`) and calls `NodeGetVolumeStats` with the volume ID and that path. A volume
  published to several allocations of a node is asked once. Calls run with a timeout and a limit on
  concurrent calls per plugin; a failure is logged at most once per volume and interval.
- The usage report of the node, which already carries the host usage to the servers
  (`RecordNodeHost` of the metrics store), gets a list of volume usages: volume ID, namespace,
  bytes used, available and total, inodes used and free, and the condition (2).
- The server keeps the latest usage per volume and node in memory (not in Raft) for the volume read,
  and samples it into the metrics store.

### 2. Volume health

- Sources: the `volume_condition` of `NodeGetVolumeStats` (node plugins with `VOLUME_CONDITION`), and
  the `volume_condition` of `ListVolumes` and `ControllerGetVolume` (controller plugins with
  `VOLUME_CONDITION`).
- The volume gets `Conditions`: per node (or `controller`) `Abnormal`, `Message` and `Since`. A
  change publishes `VolumeAbnormal` or `VolumeRecovered` on the new `CSIVolume` topic (ACL: the
  `csi-read-volume` capability of the namespace), with the volume, the namespace, the node, the
  message and the plugin. The event is published by the leader, like the `Scaling` events.
- No reaction: the allocations keep running (decision 2).

### 3. Capacity

- The `csi_plugin` block gets `capacity_interval` (duration, default `5m`, `0` off); the leader reads
  it from the task of the plugin's controller allocations.
- Per interval the leader asks one healthy controller of the plugin: once without accessible
  topology (the whole backend), and once per distinct topology segment of the plugin's node
  fingerprints, without volume capabilities or parameters. `available_capacity`,
  `maximum_volume_size` and `minimum_volume_size` go into the metrics store and into the plugin
  record (latest value).
- `norca plugin capacity` asks once with the given topology and parameters (and optionally a
  capability), for a storage class or a pool with different parameters.
- `volume create` asks with the volume's parameters, capabilities and requested topology before
  `CreateVolume` and refuses with "not enough capacity: ... available" when the plugin reports less
  than `capacity_min`. Plugins without `GET_CAPACITY` are not asked.

### 4. Live volume state

- `norca volume status <id> -live` calls `ControllerGetVolume` once and shows the plugin's view next
  to the claims.
- The volume watcher, for plugins with `GET_VOLUME` and `LIST_VOLUMES_PUBLISHED_NODES`, compares the
  published nodes with the claims at most every 5 minutes per volume and publishes `VolumeDrift` on a
  difference. It does not change anything.

### 5–7

As in the table. The checks of 7 use the capability flags of the plugin record (`SupportsClone`,
`SupportsCreateDeleteSnapshot`, `SupportsExpand`, `SupportsReadOnlyAttach`, ...).

## Phases

| Phase | Content |
| --- | --- |
| 1 | `readonly` of `ControllerPublishVolume` only with `PUBLISH_READONLY` (a spec rule that was broken); a hint in the error of a failed create from `clone_id` / `snapshot_id` when the plugin does not report the capability (the spec does not allow refusing beforehand: `CLONE_VOLUME` is "MAY report"). `NodeExpandVolume` stays as it is: the spec says the CO MUST call it when `node_expansion_required`. 5 exists already. Built 2026-11-11 (norca `feat/csi-plugin-checks`). |
| 2 | Volume statistics (1) and volume health (2): the `CSIVolume` topic, the `norca.volume.*` gauges, `volume status`. Built 2026-11-11 (norca `feat/csi-volume-stats`, PR #20; doc PR #28). As built: the calls of a plugin run one after the other with a 10 s timeout (no concurrency limit); an error is not logged but shown in the Node Usage of the volume; the conditions come only from node plugins (`NodeGetVolumeStats`), the controller side (`ListVolumes`, `ControllerGetVolume`) goes with phase 4; the usage is in `CSIVolume.NodeUsage` (per node: usage, `Abnormal`, `Message`, `At`), not in a `Conditions` list with `Since`; gauges labelled `ns`, `volume`, `plugin`, `node`. Lab case `csi` (norca-lab `case-csi-volume-stats`) PASS 42/42 on 2026-11-11: own test plugin labcsi, and the SeaweedFS CSI driver (fork bacanoo-io/seaweedfs-csi-driver with VOLUME_CONDITION) through the new registry packs seaweedfs and seaweedfs-csi (raw_exec). Found and fixed on the way: raw_exec CSI plugins get host paths; raw_exec `allowed_devices` for /dev/fuse (cgroups v2 device filter). |
| 3 | Capacity (3): `GetCapacity` in the plugin client, the periodic metric, `plugin capacity`, the check before `create`. Built 2026-11-11 (norca `feat/csi-capacity`, PR #21; doc PR #29; lab step `csi-capacity` 16/16, case `csi` 58/58). As built: answers in the leader's memory (not in the plugin record in Raft), gauges `norca.csi.*` labelled `plugin`, `topology`; the create check also refuses `capacity_min` above `maximum_volume_size`; a failed check does not block the create. |
| 4 | Live volume state (4): `ControllerGetVolume`, `volume status -live`, `VolumeDrift`. Built 2026-11-11 (norca `feat/csi-live-state`, PR #22; doc PR #30; lab step `csi-state` 16/16, case `csi` 74/74). As built: drift = claimed nodes the controller does not list, published after two checks in a row (the spec allows extra nodes in published_node_ids, they are only shown); controller conditions as VolumeAbnormal/VolumeRecovered without a node; state in the leader's memory. Lab found: taken claims keep no external node ID, taken from the node fingerprint. |
| 5 | Access modes and mount group (6). Built and merged 2026-11-11 (norca #24, doc #31, lab step `csi-modes` 18/18, case `csi` 92/92). As built: single-node-multi-writer keeps all claims on the node of the first one (claims and scheduler); the modes need SINGLE_NODE_MULTI_WRITER (scheduler filter, register/create refusal); mount_group only in the job volume block, refused in the volume specification and with block-device. |
| – | CSI spec 1.9+ (8): a separate decision |

Each phase is its own branch and pull request.

## Testing

- Unit: the plugin client against the fake CSI server of `plugins/csi` (new RPCs, capabilities,
  errors); the client's stats loop with a fake plugin; the server's capacity loop and checks; the
  claim rules of the new access modes.
- Server tests with a fake plugin: events of the `CSIVolume` topic, gauges in the metrics store.
- Lab: a CSI host-path plugin (it advertises `GET_VOLUME_STATS`, `VOLUME_CONDITION`, `GET_CAPACITY`,
  `GET_VOLUME`, `EXPAND_VOLUME`, `SINGLE_NODE_MULTI_WRITER`) on the lab VMs: statistics after writing
  into the volume, capacity per node, an abnormal condition after removing the backing directory, the
  new access modes.

## Documentation

- `job-specification/csi_plugin`: `stats_interval`, `capacity_interval`.
- `job-specification/volume` and the volume specification: the new access modes, `mount_group`.
- `commands/volume/status` (`-live`, `-abnormal`, usage), `commands/plugin/capacity` (new),
  `commands/plugin/status` (vendor, version).
- `api/volumes` (usage, conditions), `api/plugins` (capacity), `api/events` (the `CSIVolume` topic),
  `configuration/metrics_store` (the gauges).
