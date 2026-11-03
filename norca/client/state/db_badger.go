// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"bytes"
	"errors"
	"fmt"
	"hash/maphash"
	"os"
	"path/filepath"
	"strings"
	"sync"

	arstate "github.com/bacanoo-io/norca/norca/client/allocrunner/state"
	trstate "github.com/bacanoo-io/norca/norca/client/allocrunner/taskrunner/state"
	dmstate "github.com/bacanoo-io/norca/norca/client/devicemanager/state"
	"github.com/bacanoo-io/norca/norca/client/dynamicplugins"
	driverstate "github.com/bacanoo-io/norca/norca/client/pluginmanager/drivermanager/state"
	"github.com/bacanoo-io/norca/norca/client/serviceregistration/checks"
	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/helper/badgerdb"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/dgraph-io/badger/v4"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-msgpack/codec"
	"golang.org/x/crypto/blake2b"
)

/*
The client keeps its state in a Badger database in <data_dir>/client/state
(storage and RPC plan, phase 3). The keys are paths, their parts separated
by a zero byte (shown as /); values are MessagePack encoded:

	m/version                         format version (8 bytes big endian)
	a/<alloc-id>/alloc                allocEntry{*structs.Allocation}
	a/<alloc-id>/deploy_status        deployStatusEntry
	a/<alloc-id>/network_status       networkStatusEntry
	a/<alloc-id>/acknowledged_state   acknowledgedStateEntry
	a/<alloc-id>/alloc_volume         allocVolumeStatesEntry
	a/<alloc-id>/t/<task>/local_state *trstate.LocalState
	a/<alloc-id>/t/<task>/task_state  *structs.TaskState
	c/<alloc-id>_<check-id>           *structs.CheckQueryResult
	devicemanager/plugin_state        *dmstate.PluginState
	drivermanager/plugin_state        *driverstate.PluginState
	dynamicplugins/registry_state     *dynamicplugins.RegistryState
	node/meta                         map[string]*string
	node/registration                 *cstructs.NodeRegistration

Writes are durable (SyncWrites). Badger commits concurrent transactions
together with one sync, so the batch mode of the interface needs nothing
special. A write whose value did not change since the last write of the key
is skipped: the client stores the same task and allocation states often.
*/

const (
	// stateDirName is the directory of the database inside the state dir.
	stateDirName = "state"

	// stateFormatVersion is the layout version of the keys above.
	stateFormatVersion = 1
)

const sep = "\x00"

var keyStateVersion = []byte("m" + sep + "version")

func allocPrefix(allocID string) []byte { return []byte("a" + sep + allocID + sep) }

func allocKeyOf(allocID, name string) []byte { return []byte("a" + sep + allocID + sep + name) }

func taskPrefix(allocID, task string) []byte {
	return []byte("a" + sep + allocID + sep + "t" + sep + task + sep)
}

func taskKeyOf(allocID, task, name string) []byte {
	return append(taskPrefix(allocID, task), name...)
}

func checkPrefix(allocID string) []byte { return []byte("c" + sep + allocID + "_") }

func checkKeyOf(allocID string, id structs.CheckID) []byte {
	return append([]byte("c"+sep), keyForCheck(allocID, id)...)
}

func singleKey(bucket, name string) []byte { return []byte(bucket + sep + name) }

var (
	keyDevicePluginState   = singleKey("devicemanager", "plugin_state")
	keyDriverPluginState   = singleKey("drivermanager", "plugin_state")
	keyDynamicRegistry     = singleKey("dynamicplugins", "registry_state")
	keyNodeMeta            = singleKey("node", "meta")
	keyNodeRegistration    = singleKey("node", "registration")
	allocNameAlloc         = "alloc"
	allocNameDeployStatus  = "deploy_status"
	allocNameNetworkStatus = "network_status"
	allocNameAckState      = "acknowledged_state"
	allocNameVolumes       = "alloc_volume"
	taskNameLocalState     = "local_state"
	taskNameTaskState      = "task_state"
)

func keyForCheck(allocID string, checkID structs.CheckID) []byte {
	return []byte(fmt.Sprintf("%s_%s", allocID, checkID))
}

// NewStateDBFunc creates a StateDB given a state directory.
type NewStateDBFunc func(logger hclog.Logger, stateDir string) (StateDB, error)

// GetStateDBFactory returns a func for creating a StateDB: a no-op one in
// -dev mode, else Badger with opts (zero values take the client defaults).
func GetStateDBFactory(devMode bool, opts badgerdb.Options) NewStateDBFunc {
	if devMode {
		return func(hclog.Logger, string) (StateDB, error) {
			return NoopDB{}, nil
		}
	}
	return func(logger hclog.Logger, stateDir string) (StateDB, error) {
		return OpenBadgerStateDB(logger, stateDir, opts)
	}
}

// BadgerStateDB persists and restores the client state in Badger. All
// methods are safe for concurrent access.
type BadgerStateDB struct {
	db     *badgerdb.DB
	logger hclog.Logger

	// deleteLock is held for reading by writes of single keys and for
	// writing by deletes of whole prefixes, which clear their hashes.
	deleteLock sync.RWMutex

	// keyLocks serialize the writes of one key, so that the hash of the
	// last write is the hash of the stored value.
	keyLocks [64]sync.Mutex
	seed     maphash.Seed

	hashLock sync.Mutex
	hashes   map[string][blake2b.Size256]byte
}

// NewBadgerStateDB opens the state in stateDir with the client defaults.
func NewBadgerStateDB(logger hclog.Logger, stateDir string) (StateDB, error) {
	return OpenBadgerStateDB(logger, stateDir, badgerdb.Options{})
}

// OpenBadgerStateDB creates or opens the state database in
// <stateDir>/state; opts.Path is ignored.
func OpenBadgerStateDB(logger hclog.Logger, stateDir string, opts badgerdb.Options) (*BadgerStateDB, error) {
	if _, err := os.Stat(filepath.Join(stateDir, "state.db")); err == nil {
		return nil, fmt.Errorf("%s holds the BoltDB client state of an older Norca (state.db); "+
			"Norca keeps the client state in Badger now: drain the node and remove state.db", stateDir)
	}
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	opts.Path = filepath.Join(stateDir, stateDirName)
	opts.Logger = logger.Named("state")
	db, err := badgerdb.Open(opts, badgerdb.ClientDefaults)
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "Cannot acquire directory lock") || strings.Contains(msg, "Another process is using this Badger database") {
			return nil, fmt.Errorf("client state in %s is in use, is another norca agent using this data_dir? %w", stateDir, err)
		}
		return nil, fmt.Errorf("failed to open client state: %w", err)
	}
	if err := db.CheckVersion(keyStateVersion, stateFormatVersion, "client state"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &BadgerStateDB{
		db:     db,
		logger: logger,
		seed:   maphash.MakeSeed(),
		hashes: make(map[string][blake2b.Size256]byte),
	}, nil
}

func (s *BadgerStateDB) Name() string {
	return "badger"
}

// Upgrade has nothing to do: the format version is checked on open.
func (s *BadgerStateDB) Upgrade() error {
	return nil
}

// Close releases all database resources and unlocks the database directory.
func (s *BadgerStateDB) Close() error {
	return s.db.Close()
}

// --- primitives -----------------------------------------------------------------------

func encodeValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := codec.NewEncoder(&buf, structs.MsgpackHandle).Encode(v); err != nil {
		return nil, fmt.Errorf("failed to encode passed object: %v", err)
	}
	return buf.Bytes(), nil
}

func decodeValue(data []byte, v any) error {
	if err := codec.NewDecoderBytes(data, structs.MsgpackHandle).Decode(v); err != nil {
		return fmt.Errorf("failed to decode data into passed object: %v", err)
	}
	return nil
}

// put stores v under key unless the last write of key stored the same value.
func (s *BadgerStateDB) put(key []byte, v any) error {
	data, err := encodeValue(v)
	if err != nil {
		return err
	}
	sum := blake2b.Sum256(data)

	s.deleteLock.RLock()
	defer s.deleteLock.RUnlock()
	kl := &s.keyLocks[maphash.Bytes(s.seed, key)%uint64(len(s.keyLocks))]
	kl.Lock()
	defer kl.Unlock()

	s.hashLock.Lock()
	last, ok := s.hashes[string(key)]
	s.hashLock.Unlock()
	if ok && last == sum {
		return nil
	}
	if err := s.db.Update(func(txn *badger.Txn) error { return txn.Set(key, data) }); err != nil {
		return err
	}
	s.hashLock.Lock()
	s.hashes[string(key)] = sum
	s.hashLock.Unlock()
	return nil
}

// get decodes the value of key into v; found is false for a missing key.
func (s *BadgerStateDB) get(key []byte, v any) (found bool, err error) {
	err = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return item.Value(func(data []byte) error { return decodeValue(data, v) })
	})
	return found, err
}

// deleteKeys deletes the given keys and the keys under the given prefixes
// in one transaction (or more when Badger finds it too big).
func (s *BadgerStateDB) deleteKeys(keys [][]byte, prefixes ...[]byte) error {
	s.deleteLock.Lock()
	defer s.deleteLock.Unlock()

	err := s.db.View(func(txn *badger.Txn) error {
		for _, p := range prefixes {
			it := txn.NewIterator(badger.IteratorOptions{Prefix: p})
			for it.Rewind(); it.Valid(); it.Next() {
				keys = append(keys, it.Item().KeyCopy(nil))
			}
			it.Close()
		}
		return nil
	})
	if err != nil || len(keys) == 0 {
		return err
	}

	txn := s.db.NewTransaction(true)
	defer func() { txn.Discard() }()
	for _, k := range keys {
		err := txn.Delete(k)
		if errors.Is(err, badger.ErrTxnTooBig) {
			if err := txn.Commit(); err != nil {
				return err
			}
			txn = s.db.NewTransaction(true)
			err = txn.Delete(k)
		}
		if err != nil {
			return err
		}
	}
	if err := txn.Commit(); err != nil {
		return err
	}

	s.hashLock.Lock()
	for _, k := range keys {
		delete(s.hashes, string(k))
	}
	s.hashLock.Unlock()
	return nil
}

// --- allocations -----------------------------------------------------------------------

// allocEntry wraps values in the Allocations buckets
type allocEntry struct {
	Alloc *structs.Allocation
}

// deployStatusEntry wraps values for DeploymentStatus keys.
type deployStatusEntry struct {
	DeploymentStatus *structs.AllocDeploymentStatus
}

// networkStatusEntry wraps values for NetworkStatus keys.
type networkStatusEntry struct {
	NetworkStatus *structs.AllocNetworkStatus
}

// acknowledgedStateEntry wraps values in the acknowledged_state bucket, so we
// can expand it in the future if need be
type acknowledgedStateEntry struct {
	State *arstate.State
}

type allocVolumeStatesEntry struct {
	State *arstate.AllocVolumes
}

// GetAllAllocations gets all allocations persisted by this client and returns
// a map of alloc ids to errors for any allocations that could not be restored.
//
// If a fatal error was encountered it will be returned and the other two
// values will be nil.
func (s *BadgerStateDB) GetAllAllocations() ([]*structs.Allocation, map[string]error, error) {
	allocs := make([]*structs.Allocation, 0, 10)
	errs := map[string]error{}
	seen := map[string]bool{}

	prefix := []byte("a" + sep)
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{Prefix: prefix, PrefetchValues: false})
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			rest := it.Item().Key()[len(prefix):]
			i := bytes.IndexByte(rest, 0)
			if i < 0 {
				continue
			}
			allocID, name := string(rest[:i]), string(rest[i+1:])
			if _, ok := seen[allocID]; !ok {
				seen[allocID] = false
			}
			if name != allocNameAlloc {
				continue
			}
			seen[allocID] = true
			var ae allocEntry
			if err := it.Item().Value(func(v []byte) error { return decodeValue(v, &ae) }); err != nil {
				errs[allocID] = fmt.Errorf("failed to decode alloc: %v", err)
				continue
			}
			// Handle upgrade path
			ae.Alloc.Canonicalize()
			ae.Alloc.Job.Canonicalize()
			allocs = append(allocs, ae.Alloc)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	for id, hasAlloc := range seen {
		if !hasAlloc {
			errs[id] = fmt.Errorf("failed to decode alloc: key not found: %s", allocNameAlloc)
		}
	}
	return allocs, errs, nil
}

// PutAllocation stores an allocation or returns an error.
func (s *BadgerStateDB) PutAllocation(alloc *structs.Allocation, _ ...WriteOption) error {
	return s.put(allocKeyOf(alloc.ID, allocNameAlloc), &allocEntry{Alloc: alloc})
}

// PutDeploymentStatus stores an allocation's DeploymentStatus or returns an
// error.
func (s *BadgerStateDB) PutDeploymentStatus(allocID string, ds *structs.AllocDeploymentStatus) error {
	return s.put(allocKeyOf(allocID, allocNameDeployStatus), &deployStatusEntry{DeploymentStatus: ds})
}

// GetDeploymentStatus retrieves an allocation's DeploymentStatus or returns an
// error.
func (s *BadgerStateDB) GetDeploymentStatus(allocID string) (*structs.AllocDeploymentStatus, error) {
	var entry deployStatusEntry
	if _, err := s.get(allocKeyOf(allocID, allocNameDeployStatus), &entry); err != nil {
		return nil, err
	}
	return entry.DeploymentStatus, nil
}

// PutNetworkStatus stores an allocation's NetworkStatus or returns an error.
func (s *BadgerStateDB) PutNetworkStatus(allocID string, ns *structs.AllocNetworkStatus, _ ...WriteOption) error {
	return s.put(allocKeyOf(allocID, allocNameNetworkStatus), &networkStatusEntry{NetworkStatus: ns})
}

// GetNetworkStatus retrieves an allocation's NetworkStatus or returns an
// error.
func (s *BadgerStateDB) GetNetworkStatus(allocID string) (*structs.AllocNetworkStatus, error) {
	var entry networkStatusEntry
	if _, err := s.get(allocKeyOf(allocID, allocNameNetworkStatus), &entry); err != nil {
		return nil, err
	}
	return entry.NetworkStatus, nil
}

// PutAcknowledgedState stores an allocation's last acknowledged state or
// returns an error if it could not be stored.
func (s *BadgerStateDB) PutAcknowledgedState(allocID string, state *arstate.State, _ ...WriteOption) error {
	return s.put(allocKeyOf(allocID, allocNameAckState), &acknowledgedStateEntry{State: state})
}

// GetAcknowledgedState retrieves an allocation's last acknowledged state
func (s *BadgerStateDB) GetAcknowledgedState(allocID string) (*arstate.State, error) {
	var entry acknowledgedStateEntry
	if _, err := s.get(allocKeyOf(allocID, allocNameAckState), &entry); err != nil {
		return nil, err
	}
	return entry.State, nil
}

// PutAllocVolumes stores stubs of an allocation's dynamic volume mounts so they
// can be restored.
func (s *BadgerStateDB) PutAllocVolumes(allocID string, state *arstate.AllocVolumes, _ ...WriteOption) error {
	return s.put(allocKeyOf(allocID, allocNameVolumes), &allocVolumeStatesEntry{State: state})
}

// GetAllocVolumes retrieves stubs of an allocation's dynamic volume mounts so
// they can be restored.
func (s *BadgerStateDB) GetAllocVolumes(allocID string) (*arstate.AllocVolumes, error) {
	var entry allocVolumeStatesEntry
	if _, err := s.get(allocKeyOf(allocID, allocNameVolumes), &entry); err != nil {
		return nil, err
	}
	return entry.State, nil
}

// DeleteAllocationBucket deletes all state of an allocation, if any.
func (s *BadgerStateDB) DeleteAllocationBucket(allocID string, _ ...WriteOption) error {
	return s.deleteKeys(nil, allocPrefix(allocID))
}

// --- tasks --------------------------------------------------------------------------------

// GetTaskRunnerState returns the LocalState and TaskState for a
// TaskRunner. LocalState or TaskState will be nil if they do not exist.
//
// If an error is encountered both LocalState and TaskState will be nil.
func (s *BadgerStateDB) GetTaskRunnerState(allocID, taskName string) (*trstate.LocalState, *structs.TaskState, error) {
	ls := &trstate.LocalState{}
	found, err := s.get(taskKeyOf(allocID, taskName, taskNameLocalState), ls)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read local task runner state: %v", err)
	}
	if !found {
		ls = nil
	}
	ts := &structs.TaskState{}
	found, err = s.get(taskKeyOf(allocID, taskName, taskNameTaskState), ts)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read task state: %v", err)
	}
	if !found {
		ts = nil
	}
	return ls, ts, nil
}

// PutTaskRunnerLocalState stores TaskRunner's LocalState or returns an error.
func (s *BadgerStateDB) PutTaskRunnerLocalState(allocID, taskName string, val *trstate.LocalState) error {
	if err := s.put(taskKeyOf(allocID, taskName, taskNameLocalState), val); err != nil {
		return fmt.Errorf("failed to write task_runner state: %v", err)
	}
	return nil
}

// PutTaskState stores a task's state or returns an error.
func (s *BadgerStateDB) PutTaskState(allocID, taskName string, state *structs.TaskState) error {
	return s.put(taskKeyOf(allocID, taskName, taskNameTaskState), state)
}

// DeleteTaskBucket deletes the state of a task, if any.
func (s *BadgerStateDB) DeleteTaskBucket(allocID, taskName string) error {
	return s.deleteKeys(nil, taskPrefix(allocID, taskName))
}

// --- plugin managers --------------------------------------------------------------

// PutDevicePluginState stores the device manager's plugin state or returns an
// error.
func (s *BadgerStateDB) PutDevicePluginState(ps *dmstate.PluginState) error {
	return s.put(keyDevicePluginState, ps)
}

// GetDevicePluginState retrieves the device manager's plugin state; nil when
// none was stored.
func (s *BadgerStateDB) GetDevicePluginState() (*dmstate.PluginState, error) {
	ps := &dmstate.PluginState{}
	found, err := s.get(keyDevicePluginState, ps)
	if err != nil {
		return nil, fmt.Errorf("failed to read device manager plugin state: %v", err)
	}
	if !found {
		return nil, nil
	}
	return ps, nil
}

// PutDriverPluginState stores the driver manager's plugin state or returns an
// error.
func (s *BadgerStateDB) PutDriverPluginState(ps *driverstate.PluginState) error {
	return s.put(keyDriverPluginState, ps)
}

// GetDriverPluginState retrieves the driver manager's plugin state; nil when
// none was stored.
func (s *BadgerStateDB) GetDriverPluginState() (*driverstate.PluginState, error) {
	ps := &driverstate.PluginState{}
	found, err := s.get(keyDriverPluginState, ps)
	if err != nil {
		return nil, fmt.Errorf("failed to read driver manager plugin state: %v", err)
	}
	if !found {
		return nil, nil
	}
	return ps, nil
}

// PutDynamicPluginRegistryState stores the dynamic plugin registry's
// state or returns an error.
func (s *BadgerStateDB) PutDynamicPluginRegistryState(ps *dynamicplugins.RegistryState) error {
	return s.put(keyDynamicRegistry, ps)
}

// GetDynamicPluginRegistryState retrieves the dynamic plugin registry's
// state; nil when none was stored.
func (s *BadgerStateDB) GetDynamicPluginRegistryState() (*dynamicplugins.RegistryState, error) {
	ps := &dynamicplugins.RegistryState{}
	found, err := s.get(keyDynamicRegistry, ps)
	if err != nil {
		return nil, fmt.Errorf("failed to read dynamic plugin registry state: %v", err)
	}
	if !found {
		return nil, nil
	}
	return ps, nil
}

// --- checks --------------------------------------------------------------------------------

// PutCheckResult puts qr into the state store.
func (s *BadgerStateDB) PutCheckResult(allocID string, qr *structs.CheckQueryResult) error {
	return s.put(checkKeyOf(allocID, qr.ID), qr)
}

// GetCheckResults gets the check results of all allocations.
func (s *BadgerStateDB) GetCheckResults() (checks.ClientResults, error) {
	m := make(checks.ClientResults)
	prefix := []byte("c" + sep)
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{Prefix: prefix, PrefetchValues: true})
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			key := it.Item().Key()[len(prefix):]
			allocID, _, _ := bytes.Cut(key, []byte("_"))
			var qr structs.CheckQueryResult
			if err := it.Item().Value(func(v []byte) error { return decodeValue(v, &qr) }); err != nil {
				return err
			}
			m.Insert(string(allocID), &qr)
		}
		return nil
	})
	return m, err
}

// DeleteCheckResults removes the given check results of an allocation.
func (s *BadgerStateDB) DeleteCheckResults(allocID string, checkIDs []structs.CheckID) error {
	keys := make([][]byte, 0, len(checkIDs))
	for _, id := range checkIDs {
		keys = append(keys, checkKeyOf(allocID, id))
	}
	return s.deleteKeys(keys)
}

// PurgeCheckResults removes all check results of an allocation.
func (s *BadgerStateDB) PurgeCheckResults(allocID string) error {
	return s.deleteKeys(nil, checkPrefix(allocID))
}

// --- node --------------------------------------------------------------------------------

// PutNodeMeta sets dynamic node metadata for merging with the copy from the
// Client's config.
//
// This overwrites existing dynamic node metadata entirely.
func (s *BadgerStateDB) PutNodeMeta(meta map[string]*string) error {
	return s.put(keyNodeMeta, meta)
}

// GetNodeMeta retrieves node metadata for merging with the copy from
// the Client's config.
func (s *BadgerStateDB) GetNodeMeta() (map[string]*string, error) {
	m := make(map[string]*string)
	found, err := s.get(keyNodeMeta, &m)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return m, nil
}

func (s *BadgerStateDB) PutNodeRegistration(reg *cstructs.NodeRegistration) error {
	return s.put(keyNodeRegistration, reg)
}

func (s *BadgerStateDB) GetNodeRegistration() (*cstructs.NodeRegistration, error) {
	var reg cstructs.NodeRegistration
	found, err := s.get(keyNodeRegistration, &reg)
	if err != nil || !found {
		return nil, err
	}
	return &reg, nil
}
