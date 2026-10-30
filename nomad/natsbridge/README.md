<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Norca event stream → NATS bridge

`norca-natsbridge` forwards the event stream (`/v1/event/stream`) of one Norca
region to NATS. It is the first stage of the events and audit layer of the
multi-cloud setup (event stream → NATS bridge → history and audit service, see the
[development plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/nomad-fork-development-plan/index.md#multi-cloud-connectivity)).

NATS is used **only as an event and notification bridge**. Nothing in the
cluster reads from NATS; a NATS outage delays the events but affects neither
scheduling nor deployments. The optional inbound mode (below) only makes
the bridge call the Norca API sooner: the pack registries sync on their own
interval without it.

The bridge runs as a separate job in every region (one region per cloud). A
built-in output in the agent may follow later.

This is a separate Go module (`github.com/hashicorp/nomad/natsbridge`). It
depends on the fork's `api` module, the controller SDK (`../controller`, for
the Variables based leader election) and `github.com/nats-io/nats.go`
(Apache-2.0); never on the main Nomad module.

## Subjects and messages

Every event is published to

```
<prefix>.<region>.<topic>        e.g. nomad.aws-eu.Job, nomad.aws-eu.Allocation
```

The prefix is `nomad` by default (`-subject-prefix`, may contain dots, e.g.
`norca.events`). Characters a subject token cannot contain (`.`, `*`, `>`,
white space) are replaced by `_` in the region and topic.

The payload is the event JSON exactly as the server sent it, with a `Region`
field added in front:

```json
{"Region":"aws-eu","Topic":"Job","Type":"JobRegistered","Key":"web","Namespace":"default","FilterKeys":null,"Index":4711,"Payload":{"Job":{...}}}
```

The bridge does not decode and re-encode the payload, so large integers
(indexes, nanosecond timestamps) keep their precision. Headers repeat the
metadata so consumers can filter without decoding the payload:

| Header            | Value                                                        |
|-------------------|--------------------------------------------------------------|
| `Nats-Msg-Id`     | `<region>:<index>:<topic>:<type>:<namespace>:<key>`          |
| `Nomad-Region`    | region                                                       |
| `Nomad-Index`     | Raft index of the event                                      |
| `Nomad-Topic`     | topic                                                        |
| `Nomad-Type`      | event type, e.g. `JobRegistered`                             |
| `Nomad-Namespace` | namespace (if any)                                           |
| `Nomad-Key`       | key of the event, e.g. the job ID (if any)                   |

The history service subscribes to `nomad.*.>` (all regions) or
`nomad.<region>.>`, or binds a JetStream stream to these subjects; a NATS
supercluster carries them between the clouds.

## Delivery: checkpoint, resumption, at-least-once

- The bridge subscribes with `exact=true` (`StreamExact`): every event from
  the requested index on is delivered, or the server answers 410 Gone.
- After a batch (the events of one Raft index) is published — acknowledged by
  JetStream, or flushed to the server with core NATS — its index becomes the
  resume position. It is stored in a **checkpoint** at most every
  `-checkpoint-interval` (1s), and always when the bridge stops or loses the
  leadership.
- On a restart, or when another instance takes over, the bridge resumes from
  checkpoint + 1. Events published after the last checkpoint write are
  published again: delivery is **at-least-once**. With JetStream the stable
  `Nats-Msg-Id` makes the stream discard these duplicates (within its
  duplicate window, `-jetstream-duplicates`, 10m for streams the bridge
  creates). Core NATS consumers must tolerate duplicates (deduplicate by
  `Nats-Msg-Id`).
- A failed publish is retried with backoff (1s … 30s) by resubscribing from
  the last published batch. While NATS is unreachable the bridge does not read
  further; the server keeps the events in its event buffer.

### The stream of `-jetstream-create`

The bridge owns the stream it creates. On every start it brings the settings
that can change on an existing stream back to its flags: duplicate window,
replicas, `max_age`, `max_bytes`, compression and direct gets (the log says
`updated the JetStream stream`). The storage and the deny flags can only be
chosen at creation; when they differ the bridge logs a warning and keeps the
stream (recreate it to change them). A full stream discards its oldest
messages (`discard: old`), so the bridge never stops publishing because of
the limits.

Without `-jetstream-max-age` or `-jetstream-max-bytes` the stream grows until
the server's `max_file_store` (or `max_memory_store`) is reached, and then
every publish fails. Set at least one of them; `max_bytes` times the number of
streams on a server must fit in its store.

### Checkpoint store (decision)

`-checkpoint=variable` (default) keeps the checkpoint in a Nomad Variable,
`<state-prefix>/checkpoint`, by default `nomad/jobs/<job>/checkpoint` in the
namespace of the job — written with the workload identity through the task API
socket. A Variable is used because the instances of the job run on different
nodes: whichever instance becomes the leader must see the position of the
previous one; a local file would only work for a single instance. The writes
use check-and-set and never move the stored index backwards, so a former leader
that has not noticed the loss of its lease yet cannot rewind it.

`-checkpoint=file -checkpoint-path=...` keeps it in a local file (a single
instance outside the cluster, or a task with a host volume); `-checkpoint=none`
keeps it in memory only.

The bridge's own checkpoint and lease writes are Variables events too; if the
`Variables` topic is forwarded, they are skipped. When only such skipped events
advance the stream, the checkpoint is written at most once a minute, so it
stays inside the server's event buffer without the writes feeding themselves.

### 410 Gone: gap marker

If the server can no longer resume from the checkpoint (the bridge was stopped
or NATS was unreachable while more events happened than the server's
`event_buffer_size` holds), the bridge

1. publishes a **gap marker** to `<prefix>.<region>.Bridge`:

   ```json
   {"Region":"aws-eu","Topic":"Bridge","Type":"EventGap","Key":"aws-eu","Namespace":"","Index":0,
    "Payload":{"Gap":{"FromIndex":4712,"Reason":"index-gone","Time":"2026-10-24T12:00:00Z"}}}
   ```

   meaning: the events of the region from `FromIndex` up to the next event
   published for the region may be missing (`Nats-Msg-Id`
   `<region>:gap:<FromIndex>`, so a marker repeated after a restart is a
   duplicate);
2. continues with the newest events (non-exact subscription "from now"), and
   exactly again from the first batch it receives.

The history service can then reconcile the current state from the API if it
needs to. Size `event_buffer_size` of the servers for the longest NATS outage
or bridge downtime that must not produce a gap.

Note: after a cluster restore from a snapshot taken before the checkpoint (or a
dev agent restart) the Raft index is lower than the checkpoint; the server waits
for the index silently. Delete the checkpoint Variable in that case.

## Leader election

With `-leader-elect` (default) the instances elect a leader over a lease
Variable (`<state-prefix>/lease`, check-and-set, the controller SDK's
`LeaderElector`): one instance publishes, the others stand by. A stopped leader
stores its checkpoint and releases the lease, so a standby takes over at once;
a failed leader is replaced after `-lease-duration` (20s). Run the job with
`count = 2` (or more) on distinct hosts.

## Configuration

Flags, with environment variables as defaults:

| Flag | Env | Default | |
|------|-----|---------|-|
| `-addr` | `NORCA_ADDR` (`NOMAD_ADDR`) | `http://127.0.0.1:4646` | API address, or `unix://${NOMAD_SECRETS_DIR}/api.sock` |
| `-token` | `NORCA_TOKEN` (`NOMAD_TOKEN`) | | ACL token or workload identity |
| `-region` | `NORCA_REGION` (`NOMAD_REGION`) | agent's region | region of the stream and the subjects |
| `-namespace` | `NATSBRIDGE_NAMESPACE` | `*` | namespace of the events |
| `-topics` | `NATSBRIDGE_TOPICS` | `Job,Deployment,Evaluation,Allocation,Node,Service,Scaling` | `Topic` or `Topic:key`, comma separated (server-side filter) |
| `-event-types` | `NATSBRIDGE_EVENT_TYPES` | all | event types to publish (client-side filter) |
| `-start-at` | `NATSBRIDGE_START_AT` | `now` | without a checkpoint: `now` or `oldest` (oldest buffered) |
| `-nats-url` | `NATS_URL` | `nats://127.0.0.1:4222` | comma separated server URLs (user/password in the URL) |
| `-nats-creds` | `NATS_CREDS` | | credentials file (JWT + NKey) |
| `-nats-nkey` | `NATS_NKEY` | | NKey seed file |
| `-nats-token` | `NATS_TOKEN` | | token |
| `-nats-ca`, `-nats-cert`, `-nats-key` | `NATS_CA`, `NATS_CERT`, `NATS_KEY` | | TLS: CA, client certificate and key |
| `-subject-prefix` | `NATSBRIDGE_SUBJECT_PREFIX` | `nomad` | |
| `-jetstream` | `NATSBRIDGE_JETSTREAM` | `false` | publish with JetStream, wait for acks |
| `-jetstream-stream` | `NATSBRIDGE_JETSTREAM_STREAM` | | stream the messages must be stored in |
| `-jetstream-create` | `NATSBRIDGE_JETSTREAM_CREATE` | `false` | create that stream for `<prefix>.<region>.>` if missing |
| `-jetstream-duplicates` | `NATSBRIDGE_JETSTREAM_DUPLICATES` | `10m` | duplicate window of that stream |
| `-jetstream-replicas` | `NATSBRIDGE_JETSTREAM_REPLICAS` | `1` | replicas of that stream (1 to 5); 3 keeps the events when a server or its datacenter fails |
| `-jetstream-max-age` | `NATSBRIDGE_JETSTREAM_MAX_AGE` | `0` (no limit) | age after which events are dropped, e.g. `72h`; at least the duplicate window |
| `-jetstream-max-bytes` | `NATSBRIDGE_JETSTREAM_MAX_BYTES` | `0` (no limit) | size of the stream (of each replica), e.g. `512MB` |
| `-jetstream-storage` | `NATSBRIDGE_JETSTREAM_STORAGE` | `file` | `file` or `memory`; only when the stream is created |
| `-jetstream-compression` | `NATSBRIDGE_JETSTREAM_COMPRESSION` | `none` | `none` or `s2` (file storage only) |
| `-jetstream-allow-direct` | `NATSBRIDGE_JETSTREAM_ALLOW_DIRECT` | `false` | every replica answers direct gets, not only the leader |
| `-jetstream-deny-delete`, `-jetstream-deny-purge` | `NATSBRIDGE_JETSTREAM_DENY_DELETE`, `NATSBRIDGE_JETSTREAM_DENY_PURGE` | `false` | forbid deleting messages / purging; only when the stream is created |
| `-publish-timeout` | `NATSBRIDGE_PUBLISH_TIMEOUT` | `10s` | per batch |
| `-checkpoint` | `NATSBRIDGE_CHECKPOINT` | `variable` | `variable`, `file` or `none` |
| `-checkpoint-path` | `NATSBRIDGE_CHECKPOINT_PATH` | `<state-prefix>/checkpoint` | |
| `-state-namespace` | `NORCA_NAMESPACE` (`NOMAD_NAMESPACE`) | `default` | namespace of the checkpoint and lease Variables |
| `-state-prefix` | | `nomad/jobs/<NOMAD_JOB_ID>`, else `natsbridge/<region>` | |
| `-leader-elect` | `NATSBRIDGE_LEADER_ELECT` | `true` | |
| `-lease-duration` | | `20s` | |
| `-metrics-addr` | `NATSBRIDGE_METRICS_ADDR` | `127.0.0.1:9464` | `/metrics` and `/healthz`; empty disables |
| `-sync-pushes` | `NATSBRIDGE_SYNC_PUSHES` | `false` | sync pack registries on push notices (below) |
| `-sync-regions` | `NATSBRIDGE_SYNC_REGIONS` | `false` | publish pack registry events, sync on those of other regions (below) |
| `-sync-cooldown` | `NATSBRIDGE_SYNC_COOLDOWN` | `1m` | minimum time between two syncs of a registry triggered from NATS |

The default topics are those a workload identity with `read-job` and node read
may subscribe to; `ACLToken`, `ACLPolicy`, `NodePool` and `*` need a management
token.


## Inbound: pack registry syncs

The cluster pack registries (`packs.norca.io/Registry`) sync when they are
saved, on request and on their `syncInterval`; the leader fetches them. With
the inbound mode, NATS messages make the bridge sync them sooner through
the API (`POST /v1/pack-registry/:name/sync`). The bridge instances of a
region subscribe in the queue group `norca-natsbridge-sync.<region>`, so one
of them handles each message.

**Push notices** (`-sync-pushes`): whoever pushes packs, typically the
release workflow of a pack registry, publishes a notice on
`<prefix>.packs.pushed` (the region token `packs` is reserved). The bridge
syncs the cluster registries of that source (compared without the scheme
case, a trailing `/` or `.git`), or the one named:

```sh
norca-natsbridge notify-push -nats-url "$NATS_URL" -nats-creds ci.creds \
  -source oci://ghcr.io/org/packs -pack redis -version 1.2.0
```

```json
{"Source":"oci://ghcr.io/org/packs","Registry":"","Pack":"redis","Version":"1.2.0","Digest":"sha256:..."}
```

**Regions** (`-sync-regions`): the bridge also forwards the events of the
registries (topic filter `CustomObject:packs.norca.io/Registry`, added to
`-topics`) and subscribes to `<prefix>.*.CustomObject`. When the registry of
another region found other packs (versions and digests) than the local
registry of the same name, type and source, the bridge syncs the local one,
so a new pack version appears in every region at about the same time.
Registries of the same name with another source are left alone, and the
cooldown limits the syncs a registry gets from NATS.

The ACL policy (`examples/natsbridge-policy.hcl`) needs
`object "packs.norca.io/Registry" { capabilities = ["read", "write"] }` in the
`namespace "*"` block for the inbound mode.

## Metrics

`/metrics` (Prometheus text format):

| Metric | |
|--------|-|
| `norca_natsbridge_events_published_total{topic}` | events published (gap markers as topic `Bridge`) |
| `norca_natsbridge_events_duplicate_total` | events JetStream reported as duplicates |
| `norca_natsbridge_events_skipped_total{reason}` | `own` (checkpoint/lease writes), `type` (event type filter) |
| `norca_natsbridge_publish_failures_total` | events whose publish failed (retried) |
| `norca_natsbridge_stream_errors_total{reason}` | failed subscriptions, interrupted streams |
| `norca_natsbridge_gaps_total` | gap markers (410 Gone) |
| `norca_natsbridge_checkpoint_failures_total` | failed checkpoint reads and writes |
| `norca_natsbridge_received_index`, `..._published_index`, `..._checkpoint_index` | positions |
| `norca_natsbridge_lag_index` | received index − published index |
| `norca_natsbridge_lag_seconds` | age of the oldest received but unpublished batch (grows during NATS outages) |
| `norca_natsbridge_leader` | 1 on the publishing instance |

## Running it

Example job and ACL policy: `examples/natsbridge.nomad.hcl`,
`examples/natsbridge-policy.hcl`.

```
cd nomad/natsbridge
go build -o norca-natsbridge ./cmd/norca-natsbridge
./norca-natsbridge -nats-url nats://127.0.0.1:4222 -jetstream -jetstream-stream NOMAD -jetstream-create \
  -leader-elect=false -checkpoint file -checkpoint-path ./checkpoint.json
nats sub 'nomad.>'
```

SIGINT/SIGTERM stop it gracefully: the current batch is finished or abandoned
(and published again by the next run), the checkpoint is stored, the lease is
released and the NATS connection closed.

## Tests

```
go test ./...                                     # unit tests, embedded NATS server
NORCA_BINARY=/path/to/norca go test -run Integration ./...   # against `norca agent -dev`
```

The unit tests run an embedded NATS server (`github.com/nats-io/nats-server/v2`,
Apache-2.0, test dependency only) and cover JetStream deduplication, resumption
after a restart, 410 handling and leader election. The integration test starts
a dev agent with a small event buffer and runs the bridge command end to end.
