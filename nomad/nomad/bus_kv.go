// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/nats-io/nats.go/jetstream"
)

// consulKVBucket returns the key-value bucket of the Consul KV on the bus;
// nil, nil when the server runs no bus or the bucket does not exist yet.
func (s *Server) consulKVBucket(ctx context.Context) (jetstream.KeyValue, func(), error) {
	if s.bus == nil {
		return nil, func() {}, nil
	}
	if !s.bus.JetStreamReady() {
		return nil, nil, errors.New("the system bus has no JetStream meta leader")
	}
	nc, err := s.bus.Connect()
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	kv, err := js.KeyValue(ctx, bus.ConsulKVBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		nc.Close()
		return nil, func() {}, nil
	}
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return kv, nc.Close, nil
}

// exportConsulKV reads every key of the Consul KV bucket of the bus; nil
// without a bus or bucket.
func (s *Server) exportConsulKV(ctx context.Context) (*consulkv.Export, error) {
	kv, done, err := s.consulKVBucket(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if kv == nil {
		return nil, nil
	}
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	// white room bugfix: the lister is stopped also on an early return (the
	// connection closed by done ends it too, but only after the return).
	defer func() { _ = lister.Stop() }()
	out := &consulkv.Export{Bucket: bus.ConsulKVBucket, Entries: []*consulkv.Entry{}}
	for natsKey := range lister.Keys() {
		e, err := kv.Get(ctx, natsKey)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", natsKey, err)
		}
		namespace, path, key, err := consulkv.SplitScopedKey(natsKey)
		if err != nil {
			return nil, err
		}
		out.Entries = append(out.Entries, &consulkv.Entry{
			Namespace: namespace, Path: path, Key: key,
			Value: e.Value(), Revision: e.Revision(), Created: e.Created().UTC(),
		})
	}
	slices.SortFunc(out.Entries, func(a, b *consulkv.Entry) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Path, b.Path),
			strings.Compare(a.Key, b.Key))
	})
	return out, nil
}

// backupConsulKV is the ConsulKV dependency of the backups: the export of
// the bucket, or nil without a bus or bucket.
func (s *Server) backupConsulKV(ctx context.Context) ([]byte, error) {
	export, err := s.exportConsulKV(ctx)
	if err != nil || export == nil {
		return nil, err
	}
	return consulkv.Encode(export)
}

// importConsulKV writes keys of the Consul KV bucket of the bus. With replace
// the bucket is emptied first (a full restore).
func (s *Server) importConsulKV(ctx context.Context, entries []*consulkv.Entry, replace bool) error {
	kv, done, err := s.consulKVBucket(ctx)
	if err != nil {
		return err
	}
	defer done()
	if kv == nil {
		return errors.New("the system bus has no Consul KV bucket")
	}
	if replace {
		lister, err := kv.ListKeys(ctx)
		if err != nil {
			return err
		}
		// white room bugfix: the lister is stopped also on an early return.
		defer func() { _ = lister.Stop() }()
		keep := map[string]bool{}
		for _, e := range entries {
			keep[consulKVNATSKey(e)] = true
		}
		for k := range lister.Keys() {
			if !keep[k] {
				if err := kv.Delete(ctx, k); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
					return err
				}
			}
		}
	}
	for _, e := range entries {
		if _, err := kv.Put(ctx, consulKVNATSKey(e), e.Value); err != nil {
			return fmt.Errorf("writing %q: %w", e.Key, err)
		}
	}
	return nil
}

// consulKVNATSKey returns the NATS key of an entry in the bucket of the bus.
func consulKVNATSKey(e *consulkv.Entry) string {
	return consulkv.ScopePrefix(e.Namespace, e.Path) + consulkv.EncodeKey(e.Key)
}

const (
	// consulKVMigrateTimeout bounds a migration of the Consul KV.
	consulKVMigrateTimeout = 5 * time.Minute

	// busCarriedOverMarker marks a standalone store whose Consul KV keys
	// were carried over into the clustered bus.
	busCarriedOverMarker = "consul-kv-carried-over"

	// busCarryOverInterval is how often the server checks for a standalone
	// store to carry over, and retries a failed carry-over.
	busCarryOverInterval = 5 * time.Second
)

// migrateConsulKV copies the keys of the Consul KV kept in Variables below
// path in namespace into the bucket of the bus: created, never overwritten;
// the Variables are left in place. aclObj (nil: all) limits the keys to
// those it may read.
func (s *Server) migrateConsulKV(ctx context.Context, aclObj *acl.ACL, namespace, path string, dryRun bool) (*structs.ConsulKVMigrateResult, error) {
	if s.bus == nil {
		return nil, errors.New("the system bus is not enabled")
	}
	path = strings.Trim(path, "/")
	if path == "" {
		path = consulkv.DefaultPath
	}
	kv, done, err := s.consulKVBucket(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if kv == nil {
		return nil, errors.New("the system bus has no Consul KV bucket yet")
	}

	iter, err := s.fsm.State().GetVariablesByNamespaceAndPrefix(nil, namespace, path+"/")
	if err != nil {
		return nil, err
	}
	result := &structs.ConsulKVMigrateResult{}
	skip := func(key, reason string) {
		result.Skipped = append(result.Skipped, &structs.ConsulKVMigrateSkip{Key: key, Reason: reason})
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		v := raw.(*structs.VariableEncrypted)
		if aclObj != nil && !aclObj.AllowVariableOperation(namespace, v.Path, acl.VariablesCapabilityRead, nil) {
			continue
		}
		key, err := consulkv.DecodeVariablesKey(strings.TrimPrefix(v.Path, path+"/"))
		if err != nil {
			skip(v.Path, "not a key of the Consul KV: "+err.Error())
			continue
		}
		items, err := decryptBackupVariable(s.encrypter, v)
		if err != nil {
			skip(key, "cannot decrypt the Variable: "+err.Error())
			continue
		}
		value, flags, err := consulkv.DecodeVariableItems(items)
		if err != nil {
			skip(key, "invalid Variable: "+err.Error())
			continue
		}
		if flags != 0 {
			skip(key, "the bus backend does not keep flags")
			continue
		}

		natsKey := consulkv.ScopePrefix(namespace, path) + consulkv.EncodeKey(key)
		if !dryRun {
			_, err = kv.Create(ctx, natsKey, value)
			if err == nil {
				result.Copied = append(result.Copied, key)
				continue
			}
			if !errors.Is(err, jetstream.ErrKeyExists) {
				return nil, fmt.Errorf("writing %q: %w", key, err)
			}
		}
		existing, err := kv.Get(ctx, natsKey)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			result.Copied = append(result.Copied, key)
		case err != nil:
			return nil, fmt.Errorf("reading %q: %w", key, err)
		case bytes.Equal(existing.Value(), value):
			result.Unchanged = append(result.Unchanged, key)
		default:
			result.Conflicts = append(result.Conflicts, key)
		}
	}
	if !dryRun {
		s.logger.Info("system bus: migrated the Consul KV from Variables", "namespace", namespace, "path", path,
			"copied", len(result.Copied), "unchanged", len(result.Unchanged),
			"conflicts", len(result.Conflicts), "skipped", len(result.Skipped))
	}
	return result, nil
}

// busCarryOverLoop carries the Consul KV keys of a standalone bus over into
// the clustered bus: after the bus became clustered, or when it started
// clustered while the standalone store of an earlier run is still aside.
func (s *Server) busCarryOverLoop() {
	ticker := time.NewTicker(busCarryOverInterval)
	defer ticker.Stop()
	for {
		if s.bus.Clustered() {
			done, err := s.carryOverConsulKV(s.shutdownCtx)
			if err != nil {
				s.logger.Warn("system bus: carrying the Consul KV of the standalone bus over failed, retrying", "error", err)
			}
			if done {
				return
			}
		}
		select {
		case <-s.shutdownCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

// carryOverConsulKV copies the keys of the Consul KV bucket of the
// standalone store into the clustered bucket once the leader created it.
// Keys written on the cluster meanwhile are kept. done is true when there is
// nothing (more) to carry over.
func (s *Server) carryOverConsulKV(ctx context.Context) (bool, error) {
	aside := s.bus.StandaloneStoreDir()
	if aside == "" {
		return true, nil
	}
	if _, err := os.Stat(aside); os.IsNotExist(err) {
		return true, nil
	}
	marker := filepath.Join(aside, busCarriedOverMarker)
	if _, err := os.Stat(marker); err == nil {
		return true, nil
	}

	ctx, cancel := context.WithTimeout(ctx, consulKVMigrateTimeout)
	defer cancel()
	kv, done, err := s.consulKVBucket(ctx)
	if err != nil || kv == nil {
		return false, err // nil, nil: the leader has not created the bucket yet
	}
	defer done()

	entries, err := bus.ReadStoreKV(ctx, aside, bus.ConsulKVBucket, s.config.Bus.MaxStorage, s.logger)
	if err != nil {
		return false, err
	}
	copied := 0
	for _, e := range entries {
		_, err := kv.Create(ctx, e.Key, e.Value)
		if errors.Is(err, jetstream.ErrKeyExists) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("writing %q: %w", e.Key, err)
		}
		copied++
	}
	if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return false, err
	}
	s.logger.Info("system bus: carried the Consul KV of the standalone bus over",
		"keys", len(entries), "copied", copied, "kept", len(entries)-copied)
	return true, nil
}
