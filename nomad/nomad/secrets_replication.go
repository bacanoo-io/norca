// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/go-multierror"
	"golang.org/x/time/rate"

	"github.com/bacanoo-io/norca/nomad/nomad/secretsrepl"
	"github.com/bacanoo-io/norca/nomad/nomad/state"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/bacanoo-io/norca/nomad/secretstore"
	"github.com/bacanoo-io/norca/nomad/secretstore/ubix/export/kv"
	"github.com/bacanoo-io/norca/nomad/secretstore/ubix/export/storage"
)

// Replication of secrets between regions (norca-doc
// content/docs/design/secrets-plan.md, "Multi-region operation").
//
// Secrets are region-local. A secret whose metadata sets replicate = true in
// the authoritative region (the server's authoritative_region, as for the
// ACL replication) is copied to every other region of the federation, where
// it is a read-only replica: writes, deletes and metadata changes are
// refused there, and the secrets watcher dispatches no rotation job for it.
//
// The leader of every other region pulls, over the region RPC:
//
//   - Secret.ReplicationList, a blocking query on the metadata rows of the
//     replicated secrets of the authoritative region. It returns at every
//     change there and at least every SecretsReplicationInterval. Every
//     round compares all replicated paths with the local replicas by
//     version numbers, version states and the value hashes of the
//     authoritative region (SecretVersion.SourceHash: the keyed hashes of
//     two regions differ), so a round after a leader change, an offline
//     period or a restore of the region catches up completely.
//   - Secret.Replicate for every path that differs, executed by the leader
//     of the authoritative region: the current row and the values of the
//     versions this region lacks, each encrypted for this region's
//     replication public key (package secretsrepl), which the authoritative
//     leader fetches from this region itself (Secret.ReplicationKey). A nil
//     row is the tombstone of a secret that is gone there (destroyed) or no
//     longer replicated; the replica is removed.
//
// The leader decrypts the values with its replication private key, which is
// kept in the barrier of the region's secret store (protected by its seal)
// and replaced whenever the active root key of the keyring changes, and
// writes the replica through its own Raft, re-encrypted by its own barrier,
// in one entry with the metadata row: the version history of the
// authoritative region (version numbers, creation times, soft deletes and
// destroys), the values of the live versions and the keyed value hashes of
// this region. A replica never replaces a secret of the region itself, and
// an older state (a lower current version of the same secret) never replaces
// a newer one; the state store enforces both. Both sides record audit
// records (the "replicate" operation with the system accessor
// "secret-replication" and the other region, never values), and every
// replica change is a Secret topic event of its row, which carries Replica.
//
// Both internal RPCs accept only servers: the mutual TLS certificate of a
// server of the requesting region (with verify_server_hostname) and, with
// ACLs, a management token (the replication_token of the region, as for the
// ACL replication). Secret.ReplicationKey hands out a public key only and
// needs no token.

const (
	// secretReplicationIntervalDefault is the default longest time between
	// two full comparisons with the authoritative region.
	secretReplicationIntervalDefault = 5 * time.Minute

	// secretReplicationKeyPath is the barrier path of the region's
	// replication key.
	secretReplicationKeyPath = "norca/secrets-replication-key"

	// secretReplicationAccessor is the system accessor of the audit
	// records of the replication.
	secretReplicationAccessor = "secret-replication"

	// secretReplicaKeyCacheTTL bounds how long the authoritative region
	// uses a cached public key of another region before fetching it anew.
	secretReplicaKeyCacheTTL = 10 * time.Minute

	// secretReplicaFetchBatch is the number of values fetched with one
	// Secret.Replicate call.
	secretReplicaFetchBatch = 10
)

// secretsAuthoritative reports whether this region is the authoritative
// region of the secrets replication.
func (s *Server) secretsAuthoritative() bool {
	return s.config.AuthoritativeRegion == "" || s.config.AuthoritativeRegion == s.config.Region
}

// secretReplicaReadOnly refuses a change of a replica outside the
// authoritative region. In the authoritative region (after a change of the
// authoritative region) a write turns a replica into a secret of the region.
func (s *Server) secretReplicaReadOnly(existing *structs.Secret) error {
	if existing == nil || existing.Replica == nil || s.secretsAuthoritative() {
		return nil
	}
	return structs.NewErrRPCCodedf(http.StatusConflict,
		"secret %q is replicated from the authoritative region %q and is read-only in region %q; change it in region %q",
		existing.Path, existing.Replica.Region, s.config.Region, existing.Replica.Region)
}

// secretKVDataKey is the barrier key of the KV engine's data of a version
// of a secret, as the engine stores it.
func secretKVDataKey(namespace, path string, version uint64) string {
	return secretKVPrefix + "/data/" + secretKVKey(namespace, path) + "/" + strconv.FormatUint(version, 10)
}

// secretReplicaAAD is the associated data of a replicated value: it binds
// the ciphertext to both regions, the key, the secret, the version and its
// value hash.
func secretReplicaAAD(source, target, keyID, namespace, path string, version uint64, valueHash string) []byte {
	return secretsrepl.AAD("norca-secret-replica", source, target, keyID,
		namespace, path, strconv.FormatUint(version, 10), valueHash)
}

// validateServerTLSCertificateOf checks that the RPC connection was
// initiated by a server of one of the regions (with verify_server_hostname).
func validateServerTLSCertificateOf(srv *Server, ctx *RPCContext, regions ...string) error {
	var err error
	for _, region := range regions {
		if region == "" {
			continue
		}
		if err = validateTLSCertificate(srv, ctx, "server."+region+".nomad"); err == nil {
			return nil
		}
	}
	if err == nil {
		err = errors.New("no region")
	}
	return structs.NewErrRPCCodedf(http.StatusForbidden, "only servers may replicate secrets: %v", err)
}

// ---------------------------------------
// The replication key of a region

// storedReplicationKey is the replication key of the region in the barrier.
type storedReplicationKey struct {
	ID string

	// RootKeyID is the active root key of the keyring the key was created
	// with; a new key is created when it changes.
	RootKeyID string

	Private    []byte
	CreateTime time.Time
}

// secretReplicationKey returns the region's replication key from the
// barrier of the leader's core. A new key is created (and written through
// Raft) if there is none or if the active root key of the keyring changed
// since it was created: the key is rotated together with the keyring. Only
// the requests in flight use a key, so a replaced key needs no retention.
func (s *Server) secretReplicationKey() (*secretsrepl.PrivateKey, error) {
	var rootKeyID string
	if meta, err := s.State().GetActiveRootKeyMeta(nil); err == nil && meta != nil {
		rootKeyID = meta.KeyID
	}

	c, unlock, err := s.lockedSecretCore()
	if err != nil {
		return nil, err
	}
	defer unlock()

	ctx := context.Background()
	b := c.Barrier()
	entry, err := b.Get(ctx, secretReplicationKeyPath)
	if err != nil {
		return nil, err
	}
	if entry != nil && len(entry.Value) > 0 {
		var stored storedReplicationKey
		if err := json.Unmarshal(entry.Value, &stored); err != nil {
			return nil, fmt.Errorf("invalid secret replication key: %w", err)
		}
		if stored.RootKeyID == rootKeyID {
			return secretsrepl.ParsePrivateKey(stored.Private)
		}
	}

	key, err := secretsrepl.GenerateKey()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(&storedReplicationKey{
		ID:         key.ID(),
		RootKeyID:  rootKeyID,
		Private:    key.Bytes(),
		CreateTime: time.Now().UTC(),
	})
	if err != nil {
		return nil, err
	}
	if err := b.Put(ctx, &storage.Entry{Key: secretReplicationKeyPath, Value: data}); err != nil {
		return nil, err
	}
	s.logger.Named("secrets").Info("created the secret replication key of the region",
		"key_id", key.ID(), "root_key_id", rootKeyID)
	return key, nil
}

// secretReplicaPublicKey is a cached replication public key of a region.
type secretReplicaPublicKey struct {
	id      string
	key     []byte
	fetched time.Time
}

// secretReplicaPublicKey returns the replication public key keyID of
// region. The key is fetched from the region itself, not taken from the
// request, so that a server certificate alone does not get values encrypted
// for a key of its choice.
func (s *Server) secretReplicaPublicKey(region, keyID string) ([]byte, error) {
	ss := s.secrets
	ss.replicaKeysLock.Lock()
	cached := ss.replicaKeys[region]
	ss.replicaKeysLock.Unlock()
	if cached != nil && cached.id == keyID && time.Since(cached.fetched) < secretReplicaKeyCacheTTL {
		return cached.key, nil
	}

	args := &structs.SecretReplicationKeyRequest{QueryOptions: structs.QueryOptions{Region: region}}
	var reply structs.SecretReplicationKeyResponse
	if err := s.forwardRegion(region, structs.SecretReplicationKeyRPCMethod, args, &reply); err != nil {
		return nil, fmt.Errorf("failed to fetch the replication key of region %q: %w", region, err)
	}
	if reply.Region != region || secretsrepl.ValidatePublicKey(reply.PublicKey) != nil ||
		secretsrepl.KeyID(reply.PublicKey) != reply.KeyID {
		return nil, fmt.Errorf("region %q returned an invalid replication key", region)
	}
	ss.replicaKeysLock.Lock()
	if ss.replicaKeys == nil {
		ss.replicaKeys = map[string]*secretReplicaPublicKey{}
	}
	ss.replicaKeys[region] = &secretReplicaPublicKey{id: reply.KeyID, key: reply.PublicKey, fetched: time.Now()}
	ss.replicaKeysLock.Unlock()

	if reply.KeyID != keyID {
		return nil, structs.NewErrRPCCodedf(http.StatusConflict,
			"the replication key of region %q is %s, not %s", region, reply.KeyID, keyID)
	}
	return reply.PublicKey, nil
}

// ---------------------------------------
// RPC endpoints

// replicationPeer checks the region and the connection of a replication
// request in the region it is addressed to, before it is forwarded to the
// leader: a server of the requesting region, or of this region forwarding
// the request. A request for another region is checked there.
func (e *Secret) replicationPeer(requestRegion, replicaRegion string) error {
	if requestRegion != e.srv.Region() {
		return nil
	}
	if replicaRegion == "" || replicaRegion == e.srv.Region() {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid replica region %q", replicaRegion)
	}
	return validateServerTLSCertificateOf(e.srv, e.ctx, replicaRegion, e.srv.Region())
}

// authorizeReplication checks a replication request on the server that
// executes it: a management token if ACLs are enabled (the replication
// token of the requesting region), and this region must be authoritative.
func (e *Secret) authorizeReplication(args structs.RequestWithIdentity, authErr error) error {
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	aclObj, err := e.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if aclObj != nil && !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}
	if !e.srv.secretsAuthoritative() {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"region %q is not the authoritative region of the secrets", e.srv.Region())
	}
	return nil
}

// ReplicationList lists the metadata rows of the replicated secrets for the
// replication of another region. It supports blocking queries. Internal.
func (e *Secret) ReplicationList(args *structs.SecretReplicationListRequest, reply *structs.SecretReplicationListResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if err := e.replicationPeer(args.RequestRegion(), args.ReplicaRegion); err != nil {
		return err
	}
	if done, err := e.srv.forward(structs.SecretReplicationListRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricList, args)
	if err := e.authorizeReplication(args, authErr); err != nil {
		return err
	}
	defer metrics.MeasureSince([]string{"nomad", "secret", "replication_list"}, time.Now())

	return e.srv.blockingRPC(&blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			iter, err := store.Secrets(ws)
			if err != nil {
				return err
			}
			secrets := []*structs.Secret{}
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				secret := raw.(*structs.Secret)
				if secret.Replicate && secret.Replica == nil {
					secrets = append(secrets, secret.Copy())
				}
			}
			reply.Secrets = secrets
			return e.srv.setReplyQueryMeta(store, state.TableSecrets, &reply.QueryMeta)
		},
	})
}

// Replicate returns the current state of a replicated secret and the values
// of the requested live versions, encrypted for the requesting region's
// replication key. It is executed by the leader; a read of values is
// audited. A nil Secret is a tombstone. Internal.
func (e *Secret) Replicate(args *structs.SecretReplicateRequest, reply *structs.SecretReplicateResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if err := e.replicationPeer(args.RequestRegion(), args.ReplicaRegion); err != nil {
		return err
	}
	// Values are only read by the leader's core, and only the leader's
	// state decides about tombstones.
	args.AllowStale = false
	if done, err := e.srv.forward(structs.SecretReplicateRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricRead, args)
	defer metrics.MeasureSince([]string{"nomad", "secret", "replicate"}, time.Now())

	ns, path := args.SecretNamespace, args.Path
	rec := e.auditRecord(args.GetIdentity(), structs.SecretOpReplicate, ns, path)
	rec.Accessor.System = secretReplicationAccessor
	rec.PeerRegion = args.ReplicaRegion
	if err := e.authorizeReplication(args, authErr); err != nil {
		return e.finish(rec, err)
	}
	if args.KeyID == "" || ns == "" || path == "" {
		return structs.NewErrRPCCoded(http.StatusBadRequest, "replication requires a key ID, a namespace and a path")
	}

	// The requesting region's key, from that region; before the lock, as
	// it may need a request to the other region.
	var public []byte
	if len(args.Versions) > 0 {
		var err error
		if public, err = e.srv.secretReplicaPublicKey(args.ReplicaRegion, args.KeyID); err != nil {
			return err
		}
	}

	// The row and the values under the write lock, so that they belong
	// together.
	c, unlock, err := e.srv.lockedSecretCore()
	if err != nil {
		return err
	}
	defer unlock()

	store := e.srv.fsm.State()
	if reply.Index, err = store.Index(state.TableSecrets); err != nil {
		return err
	}
	row, err := store.SecretByPath(nil, ns, path)
	if err != nil {
		return err
	}
	if row == nil || !row.Replicate || row.Replica != nil {
		// The tombstone: gone or no longer replicated.
		return nil
	}
	reply.Secret = row.Copy()

	var versions []uint64
	seen := map[uint64]bool{}
	for _, v := range args.Versions {
		sv := row.Version(v)
		if sv == nil || sv.Deleted || sv.Destroyed || seen[v] || v > math.MaxInt32 {
			continue
		}
		seen[v] = true
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		return nil
	}

	rec.Version = row.CurrentVersion
	rec.Versions = versions
	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	eng := kv.New(c.Barrier(), secretKVPrefix)
	values := make([]*structs.SecretReplicaValue, 0, len(versions))
	for _, v := range versions {
		var data map[string]any
		if data, _, err = eng.Read(context.Background(), secretKVKey(ns, path), int(v)); err != nil {
			break
		}
		var plaintext []byte
		if plaintext, err = json.Marshal(data); err != nil {
			break
		}
		hash := row.Version(v).ValueHash
		var sealed []byte
		sealed, err = secretsrepl.Seal(public, plaintext,
			secretReplicaAAD(e.srv.Region(), args.ReplicaRegion, args.KeyID, ns, path, v, hash))
		clear(plaintext)
		if err != nil {
			break
		}
		values = append(values, &structs.SecretReplicaValue{Version: v, ValueHash: hash, Sealed: sealed})
	}
	if err := e.finish(rec, err); err != nil {
		return err
	}
	reply.Values = values
	return nil
}

// ReplicationKey returns the replication public key of the region, for the
// authoritative region to encrypt the replicated values for. It is executed
// by the leader, which creates the key if there is none. Internal.
func (e *Secret) ReplicationKey(args *structs.SecretReplicationKeyRequest, reply *structs.SecretReplicationKeyResponse) error {
	if err := validateServerTLSCertificateOf(e.srv, e.ctx, e.srv.Region(), e.srv.config.AuthoritativeRegion); err != nil {
		return err
	}
	args.AllowStale = false
	if done, err := e.srv.forward(structs.SecretReplicationKeyRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricRead, args)

	key, err := e.srv.secretReplicationKey()
	if err != nil {
		return secretCodedError(err)
	}
	reply.Region = e.srv.Region()
	reply.KeyID = key.ID()
	reply.PublicKey = key.PublicKey()
	return nil
}

// ---------------------------------------
// Replicas

// secretReplicaValue is a decrypted value received for a version.
type secretReplicaValue struct {
	// hash is the version's value hash in the authoritative region.
	hash  string
	items structs.SecretItems
}

// secretReplicaSame reports whether the local row is a replica of the same
// secret of the authoritative region as src.
func secretReplicaSame(local *structs.Secret, region string, src *structs.Secret) bool {
	return local != nil && local.Replica != nil && local.Replica.Region == region &&
		local.Replica.SourceCreateIndex == src.CreateIndex
}

// secretReplicaInSync reports whether the local replica matches the row of
// the authoritative region: the same secret and modify index there, the
// same versions in the same states with the same value hashes there, and a
// value for every live version.
func secretReplicaInSync(local *structs.Secret, region string, src *structs.Secret) bool {
	if !secretReplicaSame(local, region, src) ||
		local.Replica.SourceModifyIndex != src.ModifyIndex ||
		local.CurrentVersion != src.CurrentVersion ||
		len(local.Versions) != len(src.Versions) {
		return false
	}
	for i, sv := range src.Versions {
		lv := local.Versions[i]
		if lv.Version != sv.Version || lv.Deleted != sv.Deleted || lv.Destroyed != sv.Destroyed {
			return false
		}
		if sv.Destroyed {
			continue
		}
		if lv.SourceHash != sv.ValueHash || (!sv.Deleted && lv.ValueHash == "") {
			return false
		}
	}
	return true
}

// secretReplicaHasValue reports whether the local replica holds the value of
// the version sv of the authoritative region.
func secretReplicaHasValue(local *structs.Secret, same bool, sv *structs.SecretVersion) bool {
	if !same {
		return false
	}
	lv := local.Version(sv.Version)
	return lv != nil && !lv.Destroyed && lv.ValueHash != "" && lv.SourceHash == sv.ValueHash
}

// secretReplicaNeeds returns the live versions of src whose values are
// neither held by the local replica nor among values.
func secretReplicaNeeds(local *structs.Secret, region string, src *structs.Secret, values map[uint64]*secretReplicaValue) []uint64 {
	same := secretReplicaSame(local, region, src)
	var need []uint64
	for _, sv := range src.Versions {
		if sv.Deleted || sv.Destroyed {
			continue
		}
		if v := values[sv.Version]; v != nil && v.hash == sv.ValueHash {
			continue
		}
		if secretReplicaHasValue(local, same, sv) {
			continue
		}
		need = append(need, sv.Version)
	}
	return need
}

// secretWriteReplica writes the replica of the row src of the authoritative
// region on the leader's core: the version history of src, the values of
// its live versions (from values, or kept from the existing replica) and
// the value hashes of this region. It returns the versions whose values
// were written.
func (s *Server) secretWriteReplica(region string, src *structs.Secret, values map[uint64]*secretReplicaValue) (*structs.Secret, []uint64, error) {
	c, unlock, err := s.lockedSecretCore()
	if err != nil {
		return nil, nil, err
	}
	defer unlock()

	ns, path := src.Namespace, src.Path
	existing, err := s.fsm.State().SecretByPath(nil, ns, path)
	if err != nil {
		return nil, nil, err
	}
	if existing != nil && existing.Replica == nil {
		return nil, nil, fmt.Errorf("%w: a secret of this region exists at the path", structs.ErrSecretReplicaConflict)
	}
	same := secretReplicaSame(existing, region, src)
	if same && existing.CurrentVersion > src.CurrentVersion {
		return nil, nil, fmt.Errorf("%w: the replica has version %d, the authoritative region %d",
			structs.ErrSecretReplicaConflict, existing.CurrentVersion, src.CurrentVersion)
	}

	ctx, batch := secretstore.WithBatch(context.Background())
	b := c.Barrier()
	eng := kv.New(b, secretKVPrefix)
	key := secretKVKey(ns, path)
	now := time.Now().UTC()

	hashKey, created, err := secretValueHashKey(ctx, b)
	if err != nil {
		return nil, nil, err
	}
	meta := *src.SecretMeta.Copy()
	meta.Canonicalize()
	kvMeta := &kv.Metadata{
		CurrentVersion: int(src.CurrentVersion),
		MaxVersions:    meta.MaxVersions,
		CreatedTime:    now,
		UpdatedTime:    now,
		Versions:       map[int]*kv.VersionMeta{},
	}
	oldMeta, err := eng.ReadMetadata(ctx, key)
	switch {
	case err == nil:
		kvMeta.CreatedTime = oldMeta.CreatedTime
	case errors.Is(err, kv.ErrSecretNotFound):
		oldMeta = nil
	default:
		return nil, nil, err
	}

	row := &structs.Secret{
		Namespace:      ns,
		Path:           path,
		SecretMeta:     meta,
		CurrentVersion: src.CurrentVersion,
		ItemKeys:       append([]string(nil), src.ItemKeys...),
		Replica: &structs.SecretReplica{
			Region:            region,
			SourceCreateIndex: src.CreateIndex,
			SourceModifyIndex: src.ModifyIndex,
		},
		CreateTime: src.CreateTime,
		ModifyTime: now.UnixNano(),
	}
	keep := map[uint64]bool{}
	var written []uint64
	for _, sv := range src.Versions {
		if sv.Version == 0 || sv.Version > math.MaxInt32 {
			return nil, nil, fmt.Errorf("invalid secret version %d", sv.Version)
		}
		nv := &structs.SecretVersion{
			Version:    sv.Version,
			CreateTime: sv.CreateTime,
			Deleted:    sv.Deleted,
			Destroyed:  sv.Destroyed,
		}
		if !sv.Destroyed {
			nv.SourceHash = sv.ValueHash
		}
		dataKey := secretKVDataKey(ns, path, sv.Version)
		value := values[sv.Version]
		switch {
		case sv.Destroyed:
			err = b.Delete(ctx, dataKey)
		case value != nil && value.hash == sv.ValueHash:
			var data []byte
			if data, err = json.Marshal(map[string]string(value.items)); err == nil {
				err = b.Put(ctx, &storage.Entry{Key: dataKey, Value: data})
				clear(data)
			}
			nv.ValueHash = secretValueHash(hashKey, ns, path, value.items)
			written = append(written, sv.Version)
		case secretReplicaHasValue(existing, same, sv):
			nv.ValueHash = existing.Version(sv.Version).ValueHash
		case sv.Deleted:
			// A soft deleted version whose value this region never
			// received: it is fetched if it is undeleted.
			err = b.Delete(ctx, dataKey)
		default:
			return nil, nil, fmt.Errorf("missing the value of version %d", sv.Version)
		}
		if err != nil {
			return nil, nil, err
		}
		keep[sv.Version] = true
		if kvMeta.OldestVersion == 0 || int(sv.Version) < kvMeta.OldestVersion {
			kvMeta.OldestVersion = int(sv.Version)
		}
		kvMeta.Versions[int(sv.Version)] = &kv.VersionMeta{
			Version:     int(sv.Version),
			CreatedTime: time.Unix(0, sv.CreateTime).UTC(),
			Deleted:     sv.Deleted,
			Destroyed:   sv.Destroyed,
		}
		row.Versions = append(row.Versions, nv)
	}
	// The values of versions no longer retained, or of a former secret at
	// the path.
	if oldMeta != nil {
		for v := range oldMeta.Versions {
			if !keep[uint64(v)] {
				if err := b.Delete(ctx, secretKVDataKey(ns, path, uint64(v))); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	data, err := json.Marshal(kvMeta)
	if err != nil {
		return nil, nil, err
	}
	if err := b.Put(ctx, &storage.Entry{Key: secretKVMetaKey(ns, path), Value: data}); err != nil {
		return nil, nil, err
	}
	secretRowCurrent(row)

	req := &structs.SecretApplyRequest{
		Secret:      row,
		Blobs:       batch.Blobs(),
		DeleteBlobs: batch.Deletes(),
	}
	if existing != nil {
		req.ExpectedIndex = existing.ModifyIndex
	}
	if created {
		req.CreateBlobs = []string{secretHMACKeyPath}
	}
	out, _, err := s.secretApply(req)
	return out, written, err
}

// secretPurgeReplica removes a replica with all of its versions. A secret
// of the region itself is never removed.
func (s *Server) secretPurgeReplica(namespace, path string) error {
	c, unlock, err := s.lockedSecretCore()
	if err != nil {
		return err
	}
	defer unlock()

	existing, err := s.fsm.State().SecretByPath(nil, namespace, path)
	if err != nil || existing == nil || existing.Replica == nil {
		return err
	}
	ctx, batch := secretstore.WithBatch(context.Background())
	if err := kv.New(c.Barrier(), secretKVPrefix).DeleteMetadata(ctx, secretKVKey(namespace, path)); err != nil {
		return err
	}
	_, _, err = s.secretPurge(&structs.SecretPurgeRequest{
		Namespace:     namespace,
		Path:          path,
		ExpectedIndex: existing.ModifyIndex,
		DeleteBlobs:   batch.Deletes(),
	})
	return err
}

// ---------------------------------------
// The replication loop

// replicateSecrets runs the replication of secrets from the authoritative
// region until stopCh is closed, which happens when the server loses
// leadership. It does nothing in the authoritative region.
func (s *Server) replicateSecrets(stopCh chan struct{}) {
	if s.config.secretsReplicationDisabled || s.secretsAuthoritative() || s.secretStoreCore == nil {
		return
	}
	newSecretReplicator(s).run(stopCh)
}

// secretReplicator is the replication of one leadership term. It is used by
// one goroutine only.
type secretReplicator struct {
	srv    *Server
	logger hclog.Logger

	// region is the authoritative region.
	region   string
	interval time.Duration

	// warned are the paths with a conflict already logged, with the modify
	// index in the authoritative region the warning was for.
	warned map[string]uint64
}

func newSecretReplicator(s *Server) *secretReplicator {
	r := &secretReplicator{
		srv:      s,
		logger:   s.logger.Named("secrets_replication"),
		region:   s.config.AuthoritativeRegion,
		interval: s.config.SecretsReplicationInterval,
		warned:   map[string]uint64{},
	}
	if r.interval <= 0 {
		r.interval = secretReplicationIntervalDefault
	}
	return r
}

func (r *secretReplicator) run(stopCh <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	req := structs.SecretReplicationListRequest{
		ReplicaRegion: r.srv.Region(),
		QueryOptions: structs.QueryOptions{
			Region:       r.region,
			AllowStale:   true,
			MaxQueryTime: r.interval,
		},
	}
	limiter := rate.NewLimiter(replicationRateLimit, int(replicationRateLimit))
	r.logger.Debug("starting the replication of secrets from the authoritative region", "authoritative_region", r.region)

	for {
		if err := limiter.Wait(ctx); err != nil {
			return
		}
		index, err := r.round(&req)
		if err == nil {
			req.MinQueryIndex = index
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, structs.ErrNoRegionPath) {
			// The authoritative region is not (yet) known in the federation.
			r.logger.Debug("no path to the authoritative region", "authoritative_region", r.region)
		} else {
			metrics.IncrCounter([]string{"nomad", "secrets", "replication", "failed"}, 1)
			r.logger.Warn("failed to replicate secrets from the authoritative region", "error", err)
		}
		// The next round compares everything at once.
		req.MinQueryIndex = 0
		select {
		case <-time.After(r.srv.config.ReplicationBackoff):
		case <-ctx.Done():
			return
		}
	}
}

// round runs the blocking query on the replicated secrets and reconciles
// them. It returns the index of the query.
func (r *secretReplicator) round(req *structs.SecretReplicationListRequest) (uint64, error) {
	if !r.srv.hasRegionServers(r.region) {
		return 0, structs.ErrNoRegionPath
	}
	req.AuthToken = r.srv.ReplicationToken()
	var resp structs.SecretReplicationListResponse
	if err := r.srv.forwardRegion(r.region, structs.SecretReplicationListRPCMethod, req, &resp); err != nil {
		return 0, fmt.Errorf("failed to list the replicated secrets: %w", err)
	}
	return resp.Index, r.reconcile(resp.Secrets)
}

// reconcile compares the replicated secrets of the authoritative region
// with the local replicas and replicates every path that differs, including
// the replicas the authoritative region no longer lists.
func (r *secretReplicator) reconcile(remote []*structs.Secret) error {
	type pathKey struct{ namespace, path string }
	store := r.srv.State()
	listed := make(map[pathKey]struct{}, len(remote))
	var key *secretsrepl.PrivateKey
	var mErr multierror.Error

	for _, src := range remote {
		listed[pathKey{src.Namespace, src.Path}] = struct{}{}
		local, err := store.SecretByPath(nil, src.Namespace, src.Path)
		if err != nil {
			return err
		}
		if local != nil && local.Replica == nil {
			r.conflict(src, "a secret of this region exists at the path; destroy it to receive the replica")
			continue
		}
		if secretReplicaSame(local, r.region, src) && local.CurrentVersion > src.CurrentVersion {
			r.conflict(src, fmt.Sprintf("the replica has the newer version %d (version %d in the authoritative region)",
				local.CurrentVersion, src.CurrentVersion))
			continue
		}
		if secretReplicaInSync(local, r.region, src) {
			continue
		}
		if err := r.replicate(src.Namespace, src.Path, src, &key); err != nil {
			_ = multierror.Append(&mErr, fmt.Errorf("%s/%s: %w", src.Namespace, src.Path, err))
		}
	}

	// Replicas the authoritative region no longer lists: confirmed by its
	// leader, they are removed.
	iter, err := store.Secrets(nil)
	if err != nil {
		return err
	}
	var unlisted []*structs.Secret
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		sec := raw.(*structs.Secret)
		if sec.Replica == nil {
			continue
		}
		if _, ok := listed[pathKey{sec.Namespace, sec.Path}]; !ok {
			unlisted = append(unlisted, sec)
		}
	}
	for _, sec := range unlisted {
		if err := r.replicate(sec.Namespace, sec.Path, nil, &key); err != nil {
			_ = multierror.Append(&mErr, fmt.Errorf("%s/%s: %w", sec.Namespace, sec.Path, err))
		}
	}
	return mErr.ErrorOrNil()
}

// conflict logs a path that can't be replicated, once per state of the
// secret in the authoritative region.
func (r *secretReplicator) conflict(src *structs.Secret, reason string) {
	k := src.Namespace + "/" + src.Path
	if r.warned[k] == src.ModifyIndex {
		return
	}
	r.warned[k] = src.ModifyIndex
	metrics.IncrCounter([]string{"nomad", "secrets", "replication", "conflict"}, 1)
	r.logger.Warn("not replicating secret", "namespace", src.Namespace, "path", src.Path, "reason", reason)
}

// replicate brings the replica of a path up to date with the leader of the
// authoritative region: hint, if given, is its row from the list and tells
// which values to ask for first. The values are fetched in batches; a
// tombstone removes the replica. keyp caches the replication key for the
// round.
func (r *secretReplicator) replicate(namespace, path string, hint *structs.Secret, keyp **secretsrepl.PrivateKey) error {
	s := r.srv
	if *keyp == nil {
		key, err := s.secretReplicationKey()
		if err != nil {
			return err
		}
		*keyp = key
	}
	key := *keyp

	values := map[uint64]*secretReplicaValue{}
	defer func() {
		for _, v := range values {
			clear(v.items)
		}
	}()

	src := hint
	maxFetches := 3
	if hint != nil {
		maxFetches += len(hint.Versions)/secretReplicaFetchBatch + 1
	}
	for fetch := 0; fetch < maxFetches; fetch++ {
		local, err := s.State().SecretByPath(nil, namespace, path)
		if err != nil {
			return err
		}
		var need []uint64
		if src != nil {
			need = secretReplicaNeeds(local, r.region, src, values)
			if len(need) > secretReplicaFetchBatch {
				need = need[:secretReplicaFetchBatch]
			}
		}

		req := &structs.SecretReplicateRequest{
			ReplicaRegion:   s.Region(),
			KeyID:           key.ID(),
			SecretNamespace: namespace,
			Path:            path,
			Versions:        need,
			QueryOptions: structs.QueryOptions{
				Region:    r.region,
				AuthToken: s.ReplicationToken(),
			},
		}
		var reply structs.SecretReplicateResponse
		if err := s.forwardRegion(r.region, structs.SecretReplicateRPCMethod, req, &reply); err != nil {
			return fmt.Errorf("failed to fetch the secret: %w", err)
		}
		if reply.Secret == nil {
			return r.remove(namespace, path, local)
		}
		src = reply.Secret
		if src.Namespace != namespace || src.Path != path {
			return errors.New("the authoritative region returned another secret")
		}
		for _, v := range reply.Values {
			if err := r.open(key, src, v, values); err != nil {
				return err
			}
		}
		if len(secretReplicaNeeds(local, r.region, src, values)) == 0 {
			return r.write(src, values)
		}
	}
	return fmt.Errorf("the values of the secret changed during %d fetches", maxFetches)
}

// open decrypts a received value into values.
func (r *secretReplicator) open(key *secretsrepl.PrivateKey, src *structs.Secret, v *structs.SecretReplicaValue, values map[uint64]*secretReplicaValue) error {
	sv := src.Version(v.Version)
	if sv == nil || sv.ValueHash != v.ValueHash {
		return nil
	}
	plaintext, err := key.Open(v.Sealed,
		secretReplicaAAD(r.region, r.srv.Region(), key.ID(), src.Namespace, src.Path, v.Version, v.ValueHash))
	if err != nil {
		return fmt.Errorf("version %d: %w", v.Version, err)
	}
	defer clear(plaintext)
	var items structs.SecretItems
	if err := json.Unmarshal(plaintext, &items); err != nil {
		return fmt.Errorf("version %d: invalid value", v.Version)
	}
	if err := items.Validate(); err != nil {
		clear(items)
		return fmt.Errorf("version %d: %w", v.Version, err)
	}
	values[v.Version] = &secretReplicaValue{hash: v.ValueHash, items: items}
	return nil
}

// write writes the replica, with its audit record.
func (r *secretReplicator) write(src *structs.Secret, values map[uint64]*secretReplicaValue) error {
	s := r.srv
	rec := r.auditRecord(structs.SecretOpReplicate, src.Namespace, src.Path)
	rec.Version = src.CurrentVersion
	if err := s.secretAudit(rec, false); err != nil {
		return err
	}
	_, written, err := s.secretWriteReplica(r.region, src, values)
	rec.Versions = written
	rec.Result = secretAccessResult(err)
	if err != nil {
		rec.Error = err.Error()
	}
	auditErr := s.secretAudit(rec, true)
	switch {
	case errors.Is(err, structs.ErrSecretReplicaConflict):
		// A local secret or a newer version was written meanwhile.
		r.conflict(src, err.Error())
		return nil
	case err != nil:
		return err
	}
	delete(r.warned, src.Namespace+"/"+src.Path)
	metrics.IncrCounter([]string{"nomad", "secrets", "replication", "written"}, 1)
	r.logger.Debug("replicated secret", "namespace", src.Namespace, "path", src.Path,
		"version", src.CurrentVersion, "values", len(written))
	return auditErr
}

// remove removes the replica of a tombstone, with its audit record.
func (r *secretReplicator) remove(namespace, path string, local *structs.Secret) error {
	if local == nil || local.Replica == nil {
		// Nothing to remove; a secret of the region itself is kept.
		return nil
	}
	s := r.srv
	rec := r.auditRecord(structs.SecretOpDestroy, namespace, path)
	if err := s.secretAudit(rec, false); err != nil {
		return err
	}
	err := s.secretPurgeReplica(namespace, path)
	rec.Result = secretAccessResult(err)
	if err != nil {
		rec.Error = err.Error()
	}
	auditErr := s.secretAudit(rec, true)
	if err != nil {
		return err
	}
	metrics.IncrCounter([]string{"nomad", "secrets", "replication", "removed"}, 1)
	r.logger.Debug("removed secret replica", "namespace", namespace, "path", path)
	return auditErr
}

// auditRecord starts the audit record of a replica change.
func (r *secretReplicator) auditRecord(op, namespace, path string) *structs.SecretAccessEvent {
	return &structs.SecretAccessEvent{
		Region:     r.srv.Region(),
		Server:     r.srv.config.NodeName,
		Namespace:  namespace,
		Path:       path,
		Operation:  op,
		Accessor:   &structs.SecretAccessor{System: secretReplicationAccessor},
		PeerRegion: r.region,
	}
}

// hasRegionServers reports whether servers of the region are known.
func (s *Server) hasRegionServers(region string) bool {
	s.peerLock.RLock()
	defer s.peerLock.RUnlock()
	return len(s.peers[region]) > 0
}
