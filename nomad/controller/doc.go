// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package controller is the controller SDK of Norca: it helps writing
// operators that maintain custom objects following the list + watch +
// reconcile pattern.
//
// Objects are stored in one of two ways (Options.Storage):
//
//   - StorageNative: native custom objects of a kind registered with
//     "norca object-kind apply" (operator framework phase 2). The server
//     validates them against the kind's JSON Schema and CEL rules, fills in
//     default values, increases the generation on every spec change and
//     serves the status on a separate endpoint (the "write-status" ACL
//     capability), with check-and-set on the modify index. The informer
//     watches the CustomObject event topic, whose events carry the objects.
//     Controllers record the generation they acted on in the status
//     (CommonStatus.Observe sets observedGeneration).
//   - StorageVariables (the default, phase 1 prototype): objects are
//     Variables under the path convention crd/<group>/<kind>/<name>, where
//     the group is written with dashes instead of dots (Variables
//     paths do not allow dots), for example
//     crd/db-example-com/PostgresCluster/billing for the kind
//     db.example.com/PostgresCluster. The spec and the status are JSON
//     documents in the "spec" and "status" items of the same Variable, and
//     controllers record the hash of the spec they acted on instead of a
//     generation.
//
// Reconcilers see the same Object type with both storages.
//
// The building blocks are:
//
//   - Informer: lists the objects of a kind, watches the event topic of the
//     objects with exact resumption and keeps a local cache; it
//     resynchronizes by listing again when the server answers 410 Gone.
//   - Queue: a deduplicating work queue with per-key exponential backoff and
//     an overall rate limit.
//   - Controller: runs a Reconciler for the keys put on the queue by the
//     informer, by the owned jobs watch and by additional event sources, with
//     periodic resynchronization and optional leader election.
//   - LeaderElector: leader election over a lease Variable written with
//     check-and-set (Variables locks do not exist in this Nomad version), with
//     both storages.
//   - Objects: reads and writes objects, including status updates with
//     check-and-set retries.
//   - Owner references: jobs created by a controller carry the owner object in
//     their meta (OwnerMetaKey), so their changes are mapped back to it.
//
// The package depends only on the Nomad API module. Everything it needs from
// the API is behind the Backend interface, so controllers can be tested with
// fakes.
package controller
