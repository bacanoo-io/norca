// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The deletion of an object in the background or the foreground mode takes
// its dependents with it: the garbage collector deletes them, with the rights
// of the servers. For most kinds that is all there is to it. The deletion of
// a volume request that does not retain its volume, or of a snapshot schedule
// with delete_snapshots, makes the servers delete data in the storage, and
// deleting such an object needs the rights of the volume API
// (checkVolumeRequestDelete, checkSnapshotDelete). So does deleting an owner
// of it: otherwise the right to write the kind of the owner would be enough
// to have the volume or the snapshots deleted.

// objectDeletionDestroys reports whether the deletion of the object makes the
// servers delete, or let go of, data in the storage.
func objectDeletionDestroys(obj *structs.CustomObject) bool {
	switch {
	case isVolumeRequestKind(obj.Group, obj.Kind):
		return volumeRequestDeletionDestroys(obj)
	case isSnapshotKind(obj.Group, obj.Kind):
		return snapshotDeletionDestroys(obj)
	}
	return false
}

// objectDestroyingKinds are the kinds objectDeletionDestroys knows.
var objectDestroyingKinds = []string{csiVolumeRequestKind, volumeSnapshotScheduleKind}

// objectRefKey is the key of an object or of a reference to it.
func objectRefKey(kindRef, namespace, name string) string {
	return kindRef + "\x00" + namespace + "\x00" + name
}

// objectDependsOn reports whether dep is a dependent of owner, directly or
// through other objects. It follows every owner reference: the garbage
// collector deletes a dependent only when all its owners are gone, but two
// deletions that run at the same time don't see each other, so every owner
// of a dependent counts as the one whose deletion takes it.
func objectDependsOn(snap *state.StateSnapshot, dep, owner *structs.CustomObject) (bool, error) {
	target := objectRefKey(owner.KindRef(), owner.Namespace, owner.Name)
	self := objectRefKey(dep.KindRef(), dep.Namespace, dep.Name)
	if self == target {
		return false, nil
	}
	seen := map[string]struct{}{self: {}}
	queue := dep.Owners
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		if ref == nil || !ref.IsObject() {
			continue
		}
		key := objectRefKey(ref.Kind, ref.Namespace, ref.Name)
		if key == target {
			return true, nil
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		group, kind, err := structs.ParseObjectKindRef(ref.Kind)
		if err != nil {
			continue
		}
		up, err := snap.CustomObjectByName(nil, group, kind, ref.Namespace, ref.Name)
		if err != nil {
			return false, err
		}
		if up != nil {
			queue = append(queue, up.Owners...)
		}
	}
	return false, nil
}

// destroyingDependents returns the dependents of owner, direct or through
// other objects, whose deletion destroys data in the storage.
func destroyingDependents(snap *state.StateSnapshot, owner *structs.CustomObject) ([]*structs.CustomObject, error) {
	var out []*structs.CustomObject
	for _, kind := range objectDestroyingKinds {
		iter, err := snap.CustomObjects(nil, structs.OperatorsGroup, kind, structs.AllNamespacesSentinel, state.SortDefault)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			dep := raw.(*structs.CustomObject)
			if len(dep.Owners) == 0 || !objectDeletionDestroys(dep) {
				continue
			}
			depends, err := objectDependsOn(snap, dep, owner)
			if err != nil {
				return nil, err
			}
			if depends {
				out = append(out, dep)
			}
		}
	}
	return out, nil
}

// checkObjectDependentsDelete checks the deletion of an object that takes
// its dependents with it (the background and the foreground mode): the
// caller needs, for every dependent whose deletion destroys data in the
// storage, the rights that deleting that dependent itself needs. It returns
// those dependents.
func checkObjectDependentsDelete(snap *state.StateSnapshot, aclObj *acl.ACL, owner *structs.CustomObject) ([]*structs.CustomObject, error) {
	deps, err := destroyingDependents(snap, owner)
	if err != nil {
		return nil, err
	}
	allowVolume := acl.NamespaceValidator(acl.NamespaceCapabilityCSIWriteVolume)
	for _, dep := range deps {
		if allowVolume(aclObj, dep.Namespace) && aclObj.AllowPluginRead() {
			continue
		}
		// The dependent is named only to a caller who may read it.
		which := "a volume request or a snapshot schedule that depends on it"
		if aclObj.AllowObjectOperation(dep.Namespace, dep.KindRef(), acl.ObjectCapabilityRead) {
			which = "its dependent " + dep.KindRef() + " \"" + dep.Name + "\""
		}
		return nil, structs.NewErrRPCCodedf(http.StatusForbidden,
			"deleting %s %q would delete %s in the namespace %q, and with it data in the storage (its volume or its snapshots): "+
				"that needs the right %s in that namespace and to read CSI plugins; the propagation mode %q deletes the object and keeps its dependents",
			owner.KindRef(), owner.Name, which, dep.Namespace, acl.NamespaceCapabilityCSIWriteVolume, structs.ObjectPropagationOrphan)
	}
	return deps, nil
}
