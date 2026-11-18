// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"errors"
	"fmt"
	"slices"

	"github.com/bacanoo-io/norca/norca/norca/structs"
	memdb "github.com/hashicorp/go-memdb"
)

// The owner indexes find the dependents of an object without a scan of all
// objects and jobs: the objects that name it as an owner, and the jobs whose
// meta names it (structs.JobMetaOwner). The deleting index finds the objects
// marked for deletion. The object garbage collector and the deletion check
// use them.

const (
	// indexObjectOwners indexes custom objects by the objects among their
	// owners.
	indexObjectOwners = "owners"

	// indexJobOwner indexes jobs by the owner value of their meta.
	indexJobOwner = "owner"

	// indexObjectDeleting indexes custom objects by whether they are marked
	// for deletion.
	indexObjectDeleting = "deleting"
)

// objectDeleting is the condition of the deleting index.
func objectDeleting(raw any) (bool, error) {
	obj, ok := raw.(*structs.CustomObject)
	if !ok {
		return false, fmt.Errorf("unexpected type %T for custom object deleting index", raw)
	}
	return obj.Deleting(), nil
}

// objectOwnersIndexer indexes a custom object by each object it names as an
// owner, with the key of customObjectIndexer: kind reference, namespace and
// name. Owners that are not objects are not indexed.
type objectOwnersIndexer struct {
	customObjectIndexer
}

func (objectOwnersIndexer) FromObject(raw any) (bool, [][]byte, error) {
	obj, ok := raw.(*structs.CustomObject)
	if !ok {
		return false, nil, fmt.Errorf("unexpected type %T for custom object owners index", raw)
	}
	var keys [][]byte
	for _, ref := range obj.Owners {
		if !ref.IsObject() {
			continue
		}
		key := []byte(ref.Kind + "\x00" + ref.Namespace + "\x00" + ref.Name + "\x00")
		if !slices.ContainsFunc(keys, func(k []byte) bool { return string(k) == string(key) }) {
			keys = append(keys, key)
		}
	}
	return len(keys) > 0, keys, nil
}

// jobOwnerIndexer indexes a job by the owner value of its meta.
type jobOwnerIndexer struct{}

func (jobOwnerIndexer) FromObject(raw any) (bool, []byte, error) {
	job, ok := raw.(*structs.Job)
	if !ok {
		return false, nil, fmt.Errorf("unexpected type %T for job owner index", raw)
	}
	value, ok := job.Meta[structs.JobMetaOwner]
	if !ok {
		return false, nil, nil
	}
	return true, []byte(value + "\x00"), nil
}

func (jobOwnerIndexer) FromArgs(args ...any) ([]byte, error) {
	if len(args) != 1 {
		return nil, errors.New("must provide only a single argument")
	}
	value, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("argument must be a string: %#v", args[0])
	}
	return []byte(value + "\x00"), nil
}

// jobOwnerIndexSchema is the owner index of the jobs table.
func jobOwnerIndexSchema() *memdb.IndexSchema {
	return &memdb.IndexSchema{
		Name:         indexJobOwner,
		AllowMissing: true,
		Unique:       false,
		Indexer:      jobOwnerIndexer{},
	}
}

// CustomObjectsByOwner returns an iterator over the objects that name the
// object of kindRef ("group/Kind"), namespace and name as one of their
// owners.
func (s *StateStore) CustomObjectsByOwner(ws memdb.WatchSet, kindRef, namespace, name string) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableCustomObjects, indexObjectOwners, kindRef, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("custom objects lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// CustomObjectsDeleting returns an iterator over the objects marked for
// deletion, of all kinds.
func (s *StateStore) CustomObjectsDeleting(ws memdb.WatchSet) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableCustomObjects, indexObjectDeleting, true)
	if err != nil {
		return nil, fmt.Errorf("custom objects lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// WatchCustomObjects adds to the watch set a channel that fires when any
// custom object changes, without reading them.
func (s *StateStore) WatchCustomObjects(ws memdb.WatchSet) error {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableCustomObjects, indexID)
	if err != nil {
		return fmt.Errorf("custom objects lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return nil
}

// JobsByOwner returns an iterator over the jobs whose meta has the owner
// value (structs.JobMetaOwner) given, of all namespaces.
func (s *StateStore) JobsByOwner(ws memdb.WatchSet, value string) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get("jobs", indexJobOwner, value)
	if err != nil {
		return nil, fmt.Errorf("jobs lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}
