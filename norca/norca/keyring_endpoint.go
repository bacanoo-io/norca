// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/go-metrics"

	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// Keyring endpoint serves RPCs for root key management
type Keyring struct {
	srv    *Server
	ctx    *RPCContext
	logger hclog.Logger

	encrypter *Encrypter
}

func NewKeyringEndpoint(srv *Server, ctx *RPCContext, enc *Encrypter) *Keyring {
	return &Keyring{srv: srv, ctx: ctx, logger: srv.logger.Named("keyring"), encrypter: enc}
}

func (k *Keyring) Rotate(args *structs.KeyringRotateRootKeyRequest, reply *structs.KeyringRotateRootKeyResponse) error {

	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("Keyring.Rotate", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("keyring", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "keyring", "rotate"}, time.Now())

	if aclObj, err := k.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}

	if args.Algorithm == "" {
		args.Algorithm = structs.EncryptionAlgorithmAES256GCM
	}

	rootKey, err := structs.NewRootKey(args.Algorithm)
	if err != nil {
		return err
	}

	rootKey.Meta.SetActive()

	// make sure it's been added to the local keystore before we write
	// it to raft, so that followers don't try to Get a key that
	// hasn't yet been written to disk
	err = k.encrypter.AddKey(rootKey)
	if err != nil {
		return err
	}

	// Update metadata via Raft so followers can retrieve this key
	req := structs.KeyringUpdateRootKeyMetaRequest{
		RootKeyMeta:  rootKey.Meta,
		Rekey:        args.Full,
		WriteRequest: args.WriteRequest,
	}
	_, index, err := k.srv.raftApply(structs.RootKeyMetaUpsertRequestType, req)
	if err != nil {
		return err
	}
	reply.Key = rootKey.Meta
	reply.Index = index

	if args.Full {
		// like most core jobs, we don't commit this to raft b/c it's not
		// going to be periodically recreated and the ACL is from this leader
		eval := &structs.Evaluation{
			ID:          uuid.Generate(),
			Namespace:   "-",
			Priority:    structs.CoreJobPriority,
			Type:        structs.JobTypeCore,
			TriggeredBy: structs.EvalTriggerJobRegister,
			JobID:       structs.CoreJobVariablesRekey,
			Status:      structs.EvalStatusPending,
			ModifyIndex: index,
			LeaderACL:   k.srv.getLeaderAcl(),
		}
		k.srv.evalBroker.Enqueue(eval)
	}

	return nil
}

func (k *Keyring) List(args *structs.KeyringListRootKeyMetaRequest, reply *structs.KeyringListRootKeyMetaResponse) error {

	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("Keyring.List", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("keyring", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "keyring", "list"}, time.Now())

	// we need to allow both humans with management tokens and
	// non-leader servers to list keys, in order to support
	// replication
	err := validateTLSCertificateLevel(k.srv, k.ctx, tlsCertificateLevelServer)
	if err != nil {
		if aclObj, err := k.srv.ResolveACL(args); err != nil {
			return err
		} else if aclObj != nil && !aclObj.IsManagement() {
			return structs.ErrPermissionDenied
		}
	}

	// Setup the blocking query
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, s *state.StateStore) error {

			// retrieve all the key metadata
			snap, err := k.srv.fsm.State().Snapshot()
			if err != nil {
				return err
			}
			iter, err := snap.RootKeyMetas(ws)
			if err != nil {
				return err
			}

			keys := []*structs.RootKeyMeta{}
			for {
				raw := iter.Next()
				if raw == nil {
					break
				}
				keyMeta := raw.(*structs.RootKeyMeta)
				keys = append(keys, keyMeta)
			}
			reply.Keys = keys
			return k.srv.replySetIndex(state.TableRootKeyMeta, &reply.QueryMeta)
		},
	}
	return k.srv.blockingRPC(&opts)
}

// Update updates an existing key in the keyring, including both the
// key material and metadata.
func (k *Keyring) Update(args *structs.KeyringUpdateRootKeyRequest, reply *structs.KeyringUpdateRootKeyResponse) error {

	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("Keyring.Update", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("keyring", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "keyring", "update"}, time.Now())

	if aclObj, err := k.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}

	err := k.validateUpdate(args)
	if err != nil {
		return err
	}

	// make sure it's been added to the local keystore before we write
	// it to raft, so that followers don't try to Get a key that
	// hasn't yet been written to disk
	err = k.encrypter.AddKey(args.RootKey)
	if err != nil {
		return err
	}

	// unwrap the request to turn it into a meta update only
	metaReq := &structs.KeyringUpdateRootKeyMetaRequest{
		RootKeyMeta:  args.RootKey.Meta,
		WriteRequest: args.WriteRequest,
	}

	// update the metadata via Raft
	_, index, err := k.srv.raftApply(structs.RootKeyMetaUpsertRequestType, metaReq)
	if err != nil {
		return err
	}

	reply.Index = index
	return nil
}

// validateUpdate validates both the request and that any change to an
// existing key is valid
func (k *Keyring) validateUpdate(args *structs.KeyringUpdateRootKeyRequest) error {

	err := args.RootKey.Meta.Validate()
	if err != nil {
		return err
	}
	if len(args.RootKey.Key) == 0 {
		return fmt.Errorf("root key material is required")
	}

	// lookup any existing key and validate the update
	snap, err := k.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	ws := memdb.NewWatchSet()
	keyMeta, err := snap.RootKeyMetaByID(ws, args.RootKey.Meta.KeyID)
	if err != nil {
		return err
	}
	if keyMeta != nil && keyMeta.Algorithm != args.RootKey.Meta.Algorithm {
		return fmt.Errorf("root key algorithm cannot be changed after a key is created")
	}

	return nil
}

// Get retrieves an existing key from the keyring, including both the
// key material and metadata. It is used only for replication.
func (k *Keyring) Get(args *structs.KeyringGetRootKeyRequest, reply *structs.KeyringGetRootKeyResponse) error {

	authErr := k.srv.Authenticate(k.ctx, args)

	// ensure that only another server can make this request
	err := validateTLSCertificateLevel(k.srv, k.ctx, tlsCertificateLevelServer)
	if err != nil {
		return err
	}
	if done, err := k.srv.forward("Keyring.Get", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("keyring", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "keyring", "get"}, time.Now())

	if args.KeyID == "" {
		return fmt.Errorf("root key ID is required")
	}

	// Setup the blocking query
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, s *state.StateStore) error {

			// retrieve the key metadata
			snap, err := k.srv.fsm.State().Snapshot()
			if err != nil {
				return err
			}
			keyMeta, err := snap.RootKeyMetaByID(ws, args.KeyID)
			if err != nil {
				return err
			}
			if keyMeta == nil {
				return k.srv.replySetIndex(state.TableRootKeyMeta, &reply.QueryMeta)
			}

			if m := k.srv.seal; m != nil && m.realSeal() {
				// Norca: with a seal other than aead the key leaves the
				// server only wrapped by that seal
				wrapped, err := k.encrypter.wrapKeyForTransfer(keyMeta.KeyID)
				if err != nil {
					return err
				}
				wrapped.Meta = keyMeta
				reply.Key = nil
				reply.WrappedKey = wrapped
			} else {
				// retrieve the key material from the keyring
				key, err := k.encrypter.GetKey(keyMeta.KeyID)
				if err != nil {
					return err
				}
				rootKey := &structs.RootKey{
					Meta: keyMeta,
					Key:  key,
				}
				reply.Key = rootKey
			}

			// Use the last index that affected the policy table
			index, err := s.Index(state.TableRootKeyMeta)
			if err != nil {
				return err
			}

			// Ensure we never set the index to zero, otherwise a blocking query
			// cannot be used.  We floor the index at one, since realistically
			// the first write must have a higher index.
			if index == 0 {
				index = 1
			}
			reply.Index = index
			return nil
		},
	}
	return k.srv.blockingRPC(&opts)
}

func (k *Keyring) Delete(args *structs.KeyringDeleteRootKeyRequest, reply *structs.KeyringDeleteRootKeyResponse) error {

	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("Keyring.Delete", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("keyring", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "keyring", "delete"}, time.Now())

	if aclObj, err := k.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}

	if args.KeyID == "" {
		return fmt.Errorf("root key ID is required")
	}

	// lookup any existing key and validate the delete
	snap, err := k.srv.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	ws := memdb.NewWatchSet()
	keyMeta, err := snap.RootKeyMetaByID(ws, args.KeyID)
	if err != nil {
		return err
	}
	if keyMeta == nil {
		return nil // safe to bail out early
	}
	if keyMeta.Active() {
		// Norca: a refusal of the request, not a fault of the server: 409.
		return structs.NewErrRPCCoded(http.StatusConflict, "active root key cannot be deleted - call rotate first")
	}

	// Norca: a key the state still needs stays (409); the garbage collection of
	// the keyring looks at the same uses (IsRootKeyMetaInUse) before it
	// calls here. Without the key its Variables can never be read again,
	// the workload identities it signed no longer verify, and the
	// certificate authority of the native mTLS loses its private key.
	uses, err := snap.RootKeyMetaUses(args.KeyID)
	if err != nil {
		return err
	}
	switch {
	case uses.Variables > 0:
		return structs.NewErrRPCCodedf(http.StatusConflict, "root key %s encrypts %d Variable(s) - "+
			"rotate the keyring with -full first, or write or delete those Variables", args.KeyID, uses.Variables)
	case uses.Allocations > 0:
		return structs.NewErrRPCCodedf(http.StatusConflict, "root key %s signed the workload identity of %d allocation(s) that have not ended - "+
			"their tokens would no longer be accepted; remove the key when they have stopped", args.KeyID, uses.Allocations)
	case uses.MTLSRoots > 0:
		return structs.NewErrRPCCodedf(http.StatusConflict, "root key %s encrypts the private key of %d certificate authority(ies) of the native mTLS - "+
			"the leader encrypts it with the active root key after a rotation; try again shortly", args.KeyID, uses.MTLSRoots)
	}

	// Norca: a key that seals values of the Consul KV bucket of the system
	// bus stays: without it they can never be read again. The state does
	// not know of the bucket, so it is read here; when it cannot be read
	// the key is not removed on a guess.
	ctx, cancel := context.WithTimeout(k.srv.shutdownCtx, consulKVKeyCheckTimeout)
	sealed, err := k.srv.consulKVKeyIDs(ctx)
	cancel()
	if err != nil {
		// Not known now: the caller may try again (503).
		return structs.NewErrRPCCodedf(http.StatusServiceUnavailable,
			"root key %s was not removed: the Consul KV of the system bus cannot be checked for values it encrypts: %v", args.KeyID, err)
	}
	if n := sealed[args.KeyID]; n > 0 {
		return structs.NewErrRPCCodedf(http.StatusConflict, "root key %s encrypts %d value(s) of the Consul KV of the system bus - "+
			"rotate the keyring with -full first, or write or delete those keys", args.KeyID, n)
	}

	// update via Raft
	_, index, err := k.srv.raftApply(structs.RootKeyMetaDeleteRequestType, args)
	if err != nil {
		return err
	}

	// remove the key from the keyring too
	k.encrypter.RemoveKey(args.KeyID)

	reply.Index = index
	return nil
}
