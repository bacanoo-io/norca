// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package natsbridge forwards the event stream of a Norca region to NATS.
//
// NATS is used only as an event and notification bridge: it carries the
// events of the region to the history and audit service and to other
// observers, and is never in the control path of the cluster.
//
// The bridge runs as a job in every region. It subscribes to
// /v1/event/stream of its own region with exact resumption, and publishes
// every event to the subject
//
//	<prefix>.<region>.<topic>
//
// (prefix "nomad" by default), with the event JSON as received from the
// server plus a "Region" field as payload. The index of the last event
// published is kept in a checkpoint (a Variable by default), so a restarted
// bridge, or another instance that takes over the leadership, continues
// where the previous one stopped. Delivery is at-least-once: with JetStream
// every message carries a Nats-Msg-Id header that is stable across
// redeliveries, so the stream discards duplicates within its duplicate
// window.
//
// When the server can no longer resume from the checkpoint (410 Gone, the
// events were dropped from its event buffer), the bridge publishes a gap
// marker on <prefix>.<region>.Bridge and continues with the newest events.
package natsbridge
