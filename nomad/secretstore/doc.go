// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package secretstore is the storage layer of Norca's built-in secret store
// (docs/design/secrets-plan.md). The store's core comes from uBixVault
// (BSD-3-Clause), imported as source with git subtree:
//
//	nomad/secretstore/ubix/           the subtree, adapted by nomad/tools/ubixsync
//	nomad/secretstore/ubix/internal/  uBixVault's packages (barrier, shamir, seal,
//	                                  core, kv, transit, database, audit, storage)
//	nomad/secretstore/ubix/export/    generated facades: the only way the rest of
//	                                  Norca reaches the internal packages
//	nomad/secretstore/                this package: Norca's storage backend
//
// uBixVault's core sees its storage through two small interfaces of its
// storage package: a key/value Backend (Get, Put, Delete, List) and an
// HABackend that adds an HA lock electing the single active replica.
// [Backend] implements both on Norca's own Raft instead of uBixVault's
// storage backends:
//
//   - Every value the barrier writes is a [structs.SecretStoreBlob] in the
//     secret_store_blobs table of the state store, keyed by storage path. The
//     values are stored as written, which is ciphertext (the barrier encrypts
//     before it writes; only its non-secret seal configuration is plain), and
//     they are never decrypted on the FSM or state store side. Writes go
//     through Raft from the leader (message types
//     SecretStoreBlobsUpsertRequestType and SecretStoreBlobsDeleteRequestType,
//     sent with IgnoreUnknownTypeFlag) and are refused until every server of
//     the region runs a Norca version that knows them. The blobs are part of
//     every Raft snapshot, and therefore of every backup, and they are not
//     published on the event stream.
//   - Reads come from the local state store, so a follower reads what has
//     been replicated to it.
//   - The HA lock is Nomad leadership: a lock handle can be acquired only on
//     the Raft leader, after the leader has established its leadership (the
//     state store is caught up), and it is lost when leadership is revoked.
//     There is one lock per server; the lock name is ignored.
//
// # Extension points
//
// The core is not created yet; the seal (phase 1) and the store (phase 2)
// build on the following:
//
//   - The server creates the backend at startup (nomad.Server.setupSecretStore,
//     see [New] and [Deps]) and reports leadership changes to it
//     ([Backend.SetLeader] from establishLeadership and revokeLeadership).
//
//   - The core is created over it with HA, with the server's Raft ID as the
//     holder ID:
//
//     c := core.New(backend, core.WithHA(core.HAConfig{Backend: backend, HolderID: raftID}))
//     go c.RunHA(ctx, logf)
//
//     Every server unseals its own core; only the leader's core becomes
//     active, and [Backend.HALock] fences writes of a core that lost
//     leadership with storage.ErrFenced. Initialization (core.Initialize)
//     takes the lock and therefore has to run on the leader.
//
//   - Followers forward secret requests to the leader over Nomad's RPC
//     forwarding (later phases), not over uBixVault's HTTP forwarding; the
//     lock's Holder reports the leader's server ID and RPC address for
//     diagnostics only.
package secretstore
