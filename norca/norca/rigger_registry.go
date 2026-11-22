// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/rigger/catalog"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

// riggerRegistrySyncTimeout bounds a sync: the fetch of the mirror and the
// reading of the index.
const riggerRegistrySyncTimeout = 10 * time.Minute

// riggerRegistries keeps the mirrors and the indexes of the Rigger
// registries on a server. The mirrors are in the cache of Rigger
// (<data_dir>/rigger/cache); a server reads a registry at the commit of its
// last good sync, and fetches only when its mirror lacks that commit.
type riggerRegistries struct {
	srv     *Server
	fetcher *fetch.Fetcher

	// syncsMu guards syncs: the syncs of one registry run one at a time,
	// by its name. One lock for all would let a registry that does not
	// answer hold up the syncs of the others.
	syncsMu sync.Mutex
	syncs   map[string]*sync.Mutex

	// mu guards indexes: what a registry offers at a source and commit, read
	// once.
	mu      sync.Mutex
	indexes map[string]*riggerRegistryIndex

	// unpacked are the bundles of the object store unpacked into the cache
	// (by name and size), guarded by mu.
	unpacked map[string]bool
}

// riggerRegistryIndex is what a registry offers at a commit.
type riggerRegistryIndex struct {
	source, commit string
	items          []catalog.Item
}

func newRiggerRegistries(srv *Server, dir string) *riggerRegistries {
	r := &riggerRegistries{
		srv:      srv,
		fetcher:  fetch.New(filepath.Join(dir, "cache")),
		indexes:  map[string]*riggerRegistryIndex{},
		syncs:    map[string]*sync.Mutex{},
		unpacked: map[string]bool{},
	}
	r.fetcher.SetCredentials(r.credentials)

	return r
}

// credentials returns the credentials of a repository that is the source of
// a registry with an authSecret; none for every other repository (the
// sources of modules are read anonymously).
func (r *riggerRegistries) credentials(_ context.Context, repository string) (*fetch.Credentials, error) {
	iter, err := r.srv.State().CustomObjects(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", state.SortDefault)
	if err != nil {
		return nil, err
	}

	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		reg, err := riggerRegistryFromObject(raw.(*structs.CustomObject))
		if err != nil || reg.Spec.Source != repository || reg.Spec.AuthSecret == nil {
			continue
		}

		return r.srv.riggerRegistryCredentials(reg.Spec.AuthSecret)
	}

	return nil, nil
}

// riggerRegistryCredentials reads the credentials of a registry from its
// Variable: username and password (or token), ssh_user, ssh_private_key,
// ssh_key_passphrase and known_hosts.
func (s *Server) riggerRegistryCredentials(a *RiggerRegistryAuthSecret) (*fetch.Credentials, error) {
	ns := riggerAuthNamespace(a)

	v, err := s.State().GetVariable(nil, ns, a.Path)
	if err != nil {
		return nil, err
	}

	if v == nil {
		return nil, fmt.Errorf("the Variable %q does not exist in the namespace %q", a.Path, ns)
	}

	plain, err := s.encrypter.Decrypt(v.Data, v.KeyID)
	if err != nil {
		return nil, fmt.Errorf("the Variable %q can't be decrypted: %w", a.Path, err)
	}

	items := map[string]string{}
	if err := json.Unmarshal(plain, &items); err != nil {
		return nil, fmt.Errorf("the Variable %q can't be read: %w", a.Path, err)
	}

	c := &fetch.Credentials{
		Username: items["username"], Password: items["password"],
		SSHUser: items["ssh_user"], SSHKeyPassphrase: items["ssh_key_passphrase"],
	}
	if c.Password == "" {
		c.Password = items["token"]
	}

	if key := items["ssh_private_key"]; key != "" {
		c.SSHKey = []byte(key)
	}

	if hosts := items["known_hosts"]; hosts != "" {
		c.KnownHosts = []byte(hosts)
	}

	return c, nil
}

// riggerRegistriesOf returns the registries of the server, made at the first
// use in the directory of Rigger (riggerDir).
func (s *Server) riggerRegistriesOf() (*riggerRegistries, error) {
	dir, err := s.riggerDir()
	if err != nil {
		return nil, fmt.Errorf("the directory of Rigger: %w", err)
	}

	s.riggerRegistriesOnce.Do(func() { s.riggerRegistries = newRiggerRegistries(s, dir) })

	return s.riggerRegistries, nil
}

// syncRiggerRegistry syncs a registry and writes its status. It runs on the
// leader. A failed sync is written as the phase Error with its message and
// keeps what the last good sync found; it is an error of the call too.
func (s *Server) syncRiggerRegistry(name string) (*RiggerRegistry, uint64, error) {
	r, err := s.riggerRegistriesOf()
	if err != nil {
		return nil, 0, err
	}

	// A lock only for a name that is a registry, so that the names of
	// requests don't fill the map.
	found, err := s.State().CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", name)
	if err != nil {
		return nil, 0, err
	}

	if found == nil {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", structs.RiggerRegistryKindRef, name)
	}

	unlock := r.lockSync(name)
	defer unlock()

	// Read again: the sync that held the lock wrote it.
	obj, err := s.State().CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", name)
	if err != nil {
		return nil, 0, err
	}

	if obj == nil {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", structs.RiggerRegistryKindRef, name)
	}

	reg, err := riggerRegistryFromObject(obj)
	if err != nil {
		return nil, 0, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerRegistrySyncTimeout)
	defer cancel()

	now := time.Now().UTC().Format(time.RFC3339)
	status := reg.Status
	status.LastSyncedAt = now

	idx, syncErr := r.sync(ctx, reg.Spec)
	if syncErr != nil {
		status.Phase, status.Message = riggerRegistryError, operatorMessage(syncErr.Error())
	} else {
		status = riggerRegistryStatusOf(reg.Spec, idx, now)
	}

	written, err := s.writeRiggerRegistryStatus(obj, status)
	if err != nil {
		return nil, 0, err
	}

	if syncErr != nil {
		s.operatorEvent(written, "Warning", "SyncFailed", syncErr.Error())

		return nil, 0, fmt.Errorf("sync of the registry %q: %w", name, syncErr)
	}

	s.operatorEvent(written, "Normal", "Synced", fmt.Sprintf("commit %s: %d modules, %d stacks", idx.commit, status.Modules, status.Stacks))

	out, err := riggerRegistryFromObject(written)

	return out, written.ModifyIndex, err
}

// lockSync takes the lock of the syncs of a registry, and returns what
// gives it back.
func (r *riggerRegistries) lockSync(name string) func() {
	r.syncsMu.Lock()

	mu := r.syncs[name]
	if mu == nil {
		mu = &sync.Mutex{}
		r.syncs[name] = mu
	}

	r.syncsMu.Unlock()
	mu.Lock()

	return mu.Unlock
}

// riggerRegistryStatusOf is the status of a good sync.
func riggerRegistryStatusOf(spec RiggerRegistrySpec, idx *riggerRegistryIndex, now string) RiggerRegistryStatus {
	status := RiggerRegistryStatus{
		Phase: riggerRegistryReady, LastSyncedAt: now, LastGoodSyncAt: now,
		Source: idx.source, Commit: idx.commit,
	}

	for _, item := range idx.items {
		if item.Kind == "stack" {
			status.Stacks++
		} else {
			status.Modules++
		}
	}

	if spec.Display == riggerDisplaySelected {
		status.Shown, status.Missing = catalog.Select(idx.items, spec.Selected)
	}

	return status
}

// writeRiggerRegistryStatus writes the status of a registry with a
// check-and-set on the object as it was read: a registry changed during its
// sync is synced again by whoever changed it.
func (s *Server) writeRiggerRegistryStatus(obj *structs.CustomObject, status RiggerRegistryStatus) (*structs.CustomObject, error) {
	m, err := objectMap(status)
	if err != nil {
		return nil, err
	}

	out := obj.Copy()
	out.Status = m

	written, err := s.writeOperatorObject(out)
	if err != nil {
		return nil, fmt.Errorf("the status of the registry %q: %w", obj.Name, err)
	}

	return written, nil
}

// sync fetches the source of a registry now and reads its index at the
// commit of its default branch.
func (r *riggerRegistries) sync(ctx context.Context, spec RiggerRegistrySpec) (*riggerRegistryIndex, error) {
	commit, err := r.fetcher.Sync(ctx, spec.Source)
	if err != nil {
		return nil, err
	}

	return r.read(ctx, spec.Type, spec.Source, commit)
}

// read returns what a registry offers at a commit, read once per source and
// commit.
func (r *riggerRegistries) read(ctx context.Context, registryType, source, commit string) (*riggerRegistryIndex, error) {
	key := registryType + " " + source + " " + commit

	r.mu.Lock()
	idx, ok := r.indexes[key]
	r.mu.Unlock()

	if ok {
		return idx, nil
	}

	dir, err := r.fetcher.Checkout(ctx, source, commit)
	if err != nil {
		return nil, err
	}

	var items []catalog.Item

	switch registryType {
	case riggerRegistryTypeGlobal:
		items, err = catalog.Global(os.DirFS(dir))
	default:
		var tags []fetch.Tag

		tags, err = r.fetcher.TagsAt(ctx, source, commit, "")
		if err == nil {
			names := make([]string, 0, len(tags))
			for _, tag := range tags {
				names = append(names, tag.Name)
			}

			items, err = catalog.Repository(os.DirFS(dir), names)
		}
	}

	if err != nil {
		return nil, err
	}

	idx = &riggerRegistryIndex{source: source, commit: commit, items: items}

	r.mu.Lock()
	r.indexes[key] = idx
	r.mu.Unlock()

	return idx, nil
}

// errRiggerRegistryNotSynced is the error of a registry that has no good
// sync: it offers nothing.
func errRiggerRegistryNotSynced(name string) error {
	return structs.NewErrRPCCodedf(http.StatusConflict,
		"the registry %q has not been synced: sync it first (rigger registry sync %s)", name, name)
}

// searchRiggerRegistry searches the index of a registry at the commit of its
// last good sync.
func (s *Server) searchRiggerRegistry(name string, q catalog.Query) ([]catalog.Item, int, *RiggerRegistry, error) {
	obj, err := s.State().CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", name)
	if err != nil {
		return nil, 0, nil, err
	}

	if obj == nil {
		return nil, 0, nil, structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", structs.RiggerRegistryKindRef, name)
	}

	reg, err := riggerRegistryFromObject(obj)
	if err != nil {
		return nil, 0, nil, err
	}

	if reg.Status.Commit == "" {
		return nil, 0, nil, errRiggerRegistryNotSynced(name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerRegistrySyncTimeout)
	defer cancel()

	r, err := s.riggerRegistriesOf()
	if err != nil {
		return nil, 0, nil, err
	}

	idx, err := r.read(ctx, reg.Spec.Type, reg.Status.Source, reg.Status.Commit)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("the registry %q at %s: %w", name, reg.Status.Commit, err)
	}

	items, total, err := catalog.Search(idx.items, q)
	if errors.Is(err, catalog.ErrQuery) {
		return nil, 0, nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	return items, total, reg, err
}

// riggerRegistryOperator creates the global registry when it is missing.
// It syncs nothing: a sync is asked for.
type riggerRegistryOperator struct {
	srv    *Server
	logger hclog.Logger
}

func newRiggerRegistryOperator(srv *Server, logger hclog.Logger) *riggerRegistryOperator {
	return &riggerRegistryOperator{srv: srv, logger: logger.Named("rigger_registry")}
}

func (o *riggerRegistryOperator) kinds() []*structs.ObjectKind { return builtinRiggerRegistryKinds() }

// reconcile creates the registry global (the global registry of Rigger,
// shown in the UI only with what is selected for it) when it is missing.
func (o *riggerRegistryOperator) reconcile(ws memdb.WatchSet) (time.Time, error) {
	obj, err := o.srv.State().CustomObjectByName(ws, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", structs.RiggerGlobalRegistry)
	if err != nil || obj != nil {
		return time.Time{}, err
	}

	spec, err := objectMap(RiggerRegistrySpec{
		Type: riggerRegistryTypeGlobal, Source: fetch.DefaultGlobalRegistry, Display: riggerDisplaySelected,
	})
	if err != nil {
		return time.Time{}, err
	}

	created, err := o.srv.writeOperatorObject(&structs.CustomObject{
		Group: structs.RiggerObjectGroup, Kind: structs.RiggerRegistryKind, Name: structs.RiggerGlobalRegistry,
		ManagedBy: structs.OperatorsManagedBy, Spec: spec,
	})
	if err != nil {
		return time.Time{}, err
	}

	operatorCount(structs.RiggerRegistryKind, "creates")
	o.srv.operatorEvent(created, "Normal", "Created", "the global registry of Rigger, not synced yet (rigger registry sync global)")

	return time.Time{}, nil
}
