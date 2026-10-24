// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package controller is the controller SDK of Norca: it helps writing
// operators that maintain custom objects following the list + watch +
// reconcile pattern.
//
// This is the phase 1 ("prototype") version of the operator framework. Custom
// objects are stored as Variables under the path convention
//
//	crd/<group>/<kind>/<name>
//
// where the group is written with dashes instead of dots (Variables paths do
// not allow dots), for example crd/db-example-com/PostgresCluster/billing for
// the kind db.example.com/PostgresCluster. The desired state ("spec") and the
// state observed by the controller ("status") are kept as JSON documents in
// the "spec" and "status" items of the same Variable.
//
// The building blocks are:
//
//   - Informer: lists the objects of a kind, watches the Variables event topic
//     with exact resumption and keeps a local cache; it resynchronizes by
//     listing again when the server answers 410 Gone.
//   - Queue: a deduplicating work queue with per-key exponential backoff and
//     an overall rate limit.
//   - Controller: runs a Reconciler for the keys put on the queue by the
//     informer, by the owned jobs watch and by additional event sources, with
//     periodic resynchronization and optional leader election.
//   - LeaderElector: leader election over a lease Variable written with
//     check-and-set (Variables locks do not exist in this Nomad version).
//   - Objects: reads and writes objects, including status updates with
//     check-and-set retries.
//   - Owner references: jobs created by a controller carry the owner object in
//     their meta (OwnerMetaKey), so their changes are mapped back to it.
//
// The package depends only on the Nomad API module. Everything it needs from
// the API is behind the Backend interface, so controllers can be tested with
// fakes.
package controller
