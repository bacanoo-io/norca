// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Loaded is a backup decrypted into a separate in-memory state store, for
// browsing, comparison and selective restore. The running cluster is not
// touched.
type Loaded struct {
	Record *structs.BackupRecord

	// Destination the backup was read from.
	Destination string

	State *state.StateStore

	// Keys are the root keys of the backup; nil if the backup has no
	// keyring object.
	Keys []*structs.RootKey

	// SecretStoreKey is the key of the secret store core of the backup
	// (Norca); nil if the backup has no secret store key object, as backups
	// taken before Norca wrote it.
	SecretStoreKey []byte

	// ConsulKV is the Consul KV bucket of the system bus in the backup
	// (Norca); nil if the backup has none (no bus, or no bucket yet).
	ConsulKV *consulkv.Export
}

// RestoreFunc restores a Raft snapshot archive into a new state store.
type RestoreFunc func(archive io.Reader) (*state.StateStore, error)

// loadCache keeps the most recently loaded backup for a short time, so
// browsing several content types does not download and decrypt the backup
// every time. The key includes a hash of the identity, so a cached backup is
// only served to requests that could decrypt it.
type loadCache struct {
	// loadMu serializes the loads; mu guards the cached backup only, so a
	// cache hit does not wait for the load of another backup.
	loadMu sync.Mutex

	mu     sync.Mutex
	key    string
	loaded *Loaded
	timer  *time.Timer
}

// lookup returns the cached backup of key, or nil.
func (c *loadCache) lookup(key string) *Loaded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.get(key)
}

func (c *loadCache) get(key string) *Loaded {
	if c.key == key {
		return c.loaded
	}
	return nil
}

func (c *loadCache) put(key string, loaded *Loaded, ttl time.Duration) {
	if c.timer != nil {
		c.timer.Stop()
	}
	c.key, c.loaded = key, loaded
	if ttl <= 0 {
		c.key, c.loaded = "", nil
		return
	}
	c.timer = time.AfterFunc(ttl, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.key == key {
			c.key, c.loaded = "", nil
		}
	})
}

// Load downloads, verifies and decrypts a backup with the identity file
// content supplied by an administrator, and restores it into a separate
// state store. The identity is neither stored nor logged. Loads are
// serialized, so at most one backup is held in memory besides the cache.
func (m *Manager) Load(ctx context.Context, record *structs.BackupRecord, destName, identity string, restore RestoreFunc) (*Loaded, error) {
	ids, err := ParseIdentities(identity)
	if err != nil {
		return nil, err
	}
	dest, _, err := m.ReadDestination(record, destName)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256([]byte(record.ID + "\x00" + dest.Name() + "\x00" + identity))
	key := hex.EncodeToString(sum[:])

	// white room bugfix: the cache is checked under its own lock; only the
	// loads are serialized, so a cache hit no longer waits for the download
	// and restore of another backup.
	if loaded := m.cache.lookup(key); loaded != nil {
		return loaded, nil
	}
	m.cache.loadMu.Lock()
	defer m.cache.loadMu.Unlock()
	// Loaded meanwhile by the request this one waited for.
	if loaded := m.cache.lookup(key); loaded != nil {
		return loaded, nil
	}

	loaded := &Loaded{Record: record, Destination: dest.Name()}

	rc, err := m.Open(ctx, record, dest.Name(), structs.BackupObjectState, ids)
	if err != nil {
		return nil, err
	}
	store, err := restore(rc)
	if err == nil {
		// Make sure the whole object was read, so its checksum is verified.
		_, err = io.Copy(io.Discard, rc)
	}
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to load backup %s: %w", record.ID, err)
	}
	loaded.State = store

	rc, err = m.Open(ctx, record, dest.Name(), structs.BackupObjectKeyring, ids)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return nil, err
	default:
		loaded.Keys, err = DecodeKeyring(rc)
		if err == nil {
			_, err = io.Copy(io.Discard, rc)
		}
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to load the keyring of backup %s: %w", record.ID, err)
		}
	}

	rc, err = m.Open(ctx, record, dest.Name(), structs.BackupObjectSecretStoreKey, ids)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return nil, err
	default:
		loaded.SecretStoreKey, err = DecodeSecretStoreKey(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to load the secret store key of backup %s: %w", record.ID, err)
		}
	}

	rc, err = m.Open(ctx, record, dest.Name(), structs.BackupObjectConsulKV, ids)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return nil, err
	default:
		loaded.ConsulKV, err = consulkv.Decode(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to load the Consul KV of backup %s: %w", record.ID, err)
		}
	}

	m.cache.mu.Lock()
	m.cache.put(key, loaded, m.Config().BrowseCacheTTL)
	m.cache.mu.Unlock()
	return loaded, nil
}
