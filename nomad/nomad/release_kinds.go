// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Releases and their revisions are custom objects of two built-in kinds of
// the group packs.norca.io. The first release operation registers the kinds
// (and updates them after an upgrade changed them); they and their
// objects can't be written through the generic object API, only through the
// Release endpoint, which applies its own ACL checks. Reading them through
// the object API works as for every kind.

const (
	// releaseDriftInterval is how often the leader checks the deployed
	// releases for drift.
	releaseDriftInterval = 5 * time.Minute
)

// releaseKindSchema is the JSON Schema of release objects.
const releaseKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "properties": {
        "pack": {"type": "object"},
        "description": {"type": "string"},
        "revision": {"type": "integer", "minimum": 0},
        "maxHistory": {"type": "integer", "minimum": 1},
        "atomic": {"type": "boolean"},
        "multiregion": {"type": "object"}
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "phase": {"type": "string"},
        "revision": {"type": "integer", "minimum": 0},
        "resources": {"type": "array", "items": {"type": "object"}},
        "conditions": {"type": "array", "items": {"type": "object"}}
      }
    }
  }
}`

// releaseRevisionKindSchema is the JSON Schema of revision objects.
const releaseRevisionKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["release", "revision", "action", "content"],
      "properties": {
        "release": {"type": "string"},
        "revision": {"type": "integer", "minimum": 1},
        "action": {"type": "string", "enum": ["install", "upgrade", "rollback"]},
        "rollbackOf": {"type": "integer", "minimum": 0},
        "pack": {"type": "object"},
        "values": {"type": "object"},
        "variablesSchemas": {"type": "array", "items": {"type": "object"}},
        "multiregion": {"type": "object"},
        "resources": {"type": "array", "items": {"type": "object"}},
        "content": {"type": "string"},
        "contentDigest": {"type": "string"}
      }
    },
    "status": {
      "type": "object",
      "properties": {"phase": {"type": "string"}}
    }
  }
}`

// packRegistryKindSchema is the JSON Schema of cluster pack registry objects.
const packRegistryKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["type", "source"],
      "properties": {
        "type": {"type": "string", "enum": ["dir", "git", "oci", "bundle", "uploads"]},
        "source": {"type": "string"},
        "defaultRef": {"type": "string"},
        "syncInterval": {"type": "string"},
        "authSecret": {
          "type": "object",
          "required": ["path"],
          "properties": {
            "namespace": {"type": "string"},
            "path": {"type": "string", "minLength": 1}
          },
          "additionalProperties": false
        },
        "trustedKeys": {"type": "array", "items": {"type": "string"}}
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "phase": {"type": "string"},
        "message": {"type": "string"},
        "lastSyncedAt": {"type": "string"},
        "lastGoodSyncAt": {"type": "string"},
        "syncGeneration": {"type": "integer", "minimum": 0},
        "packCount": {"type": "integer", "minimum": 0},
        "packs": {"type": "array", "items": {"type": "object"}},
        "uploads": {"type": "array", "items": {"type": "object"}}
      }
    }
  }
}`

// packUploadKindSchema is the JSON Schema of the chunks of uploaded packs.
const packUploadKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["registry", "digest", "chunk", "chunks", "data"],
      "properties": {
        "registry": {"type": "string"},
        "digest": {"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"},
        "fileName": {"type": "string"},
        "format": {"type": "string", "enum": ["bundle", "archive"]},
        "size": {"type": "integer", "minimum": 0},
        "chunk": {"type": "integer", "minimum": 0},
        "chunks": {"type": "integer", "minimum": 1},
        "uploadedAt": {"type": "string"},
        "data": {"type": "string"}
      }
    }
  }
}`

// builtinReleaseKinds returns the built-in object kinds of releases and pack registries.
func builtinReleaseKinds() []*structs.ObjectKind {
	return []*structs.ObjectKind{
		{
			Group:       structs.ReleaseObjectGroup,
			Kind:        structs.ReleaseKind,
			Description: "Installation of a pack (built in, managed by the release API)",
			Namespaced:  true,
			Schema:      releaseKindSchema,
			Columns: []*structs.ObjectKindColumn{
				{Name: "Pack", Path: "spec.pack.name"},
				{Name: "Version", Path: "spec.pack.version"},
				{Name: "Revision", Path: "status.revision"},
				{Name: "Phase", Path: "status.phase"},
				{Name: "Drifted", Path: "status.conditions[?type=='Drifted'].status"},
			},
		},
		{
			Group:       structs.ReleaseObjectGroup,
			Kind:        structs.ReleaseRevisionKind,
			Description: "Immutable revision of a pack release (built in, managed by the release API)",
			Namespaced:  true,
			Schema:      releaseRevisionKindSchema,
			Rules: []*structs.ObjectKindRule{{
				Expression: "!has(oldSelf) || self.spec == oldSelf.spec",
				Message:    "revisions are immutable",
			}},
			Columns: []*structs.ObjectKindColumn{
				{Name: "Release", Path: "spec.release"},
				{Name: "Revision", Path: "spec.revision"},
				{Name: "Action", Path: "spec.action"},
				{Name: "Pack", Path: "spec.pack.name"},
				{Name: "Phase", Path: "status.phase"},
			},
		},
		{
			Group:       structs.ReleaseObjectGroup,
			Kind:        structs.PackRegistryKind,
			Description: "Cluster pack registry and catalog source (built in, managed by the pack registry API)",
			Namespaced:  false,
			Schema:      packRegistryKindSchema,
			Columns: []*structs.ObjectKindColumn{
				{Name: "Type", Path: "spec.type"},
				{Name: "Source", Path: "spec.source"},
				{Name: "Phase", Path: "status.phase"},
				{Name: "Packs", Path: "status.packCount"},
				{Name: "LastSynced", Path: "status.lastSyncedAt"},
			},
		},
		{
			Group:       structs.ReleaseObjectGroup,
			Kind:        structs.PackUploadKind,
			Description: "Chunk of a pack uploaded to a cluster pack registry (built in, managed by the pack upload API)",
			Namespaced:  false,
			Schema:      packUploadKindSchema,
			Columns: []*structs.ObjectKindColumn{
				{Name: "Registry", Path: "spec.registry"},
				{Name: "File", Path: "spec.fileName"},
				{Name: "Chunk", Path: "spec.chunk"},
				{Name: "Chunks", Path: "spec.chunks"},
			},
		},
	}
}

// isBuiltinObjectGroup reports whether objects and kinds of the group are
// built in and can't be written through the generic object API.
func isBuiltinObjectGroup(group string) bool {
	return group == structs.ReleaseObjectGroup
}

// releaseManager holds the leader-side state of release operations.
type releaseManager struct {
	srv    *Server
	logger hclog.Logger

	// lock guards busy, the releases with a running operation, kindsReady,
	// and packCacheDir.
	lock         sync.Mutex
	busy         map[string]struct{}
	kindsReady   bool
	packCacheDir string

	// syncLock serializes the syncs of pack registries, which replace
	// directories of the pack cache.
	syncLock sync.Mutex
}

func newReleaseManager(srv *Server, logger hclog.Logger) *releaseManager {
	return &releaseManager{
		srv:    srv,
		logger: logger.Named("release"),
		busy:   make(map[string]struct{}),
	}
}

// acquire marks an operation on the release as running. It fails with 409
// Conflict if another operation is running.
func (m *releaseManager) acquire(namespace, name string) (func(), error) {
	key := namespace + "/" + name
	m.lock.Lock()
	defer m.lock.Unlock()
	if _, ok := m.busy[key]; ok {
		return nil, structs.NewReleaseConflictError("another operation on release %q in namespace %q is running", name, namespace)
	}
	m.busy[key] = struct{}{}
	return func() {
		m.lock.Lock()
		delete(m.busy, key)
		m.lock.Unlock()
	}, nil
}

// isBusy reports whether an operation on the release is running.
func (m *releaseManager) isBusy(namespace, name string) bool {
	m.lock.Lock()
	defer m.lock.Unlock()
	_, ok := m.busy[namespace+"/"+name]
	return ok
}

// ensureKinds registers the built-in kinds, or updates them if they differ
// from the built-in definitions. It runs on the leader.
func (m *releaseManager) ensureKinds() error {
	m.lock.Lock()
	ready := m.kindsReady
	m.lock.Unlock()
	if ready {
		return nil
	}
	if !ServersMeetMinimumNorcaVersion(m.srv.serf.Members(), m.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to use releases", minNorcaFeaturesVersion)
	}

	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return err
	}
	for _, kind := range builtinReleaseKinds() {
		existing, err := snap.ObjectKindByName(nil, kind.Group, kind.Kind)
		if err != nil {
			return err
		}
		if existing != nil && objectKindDefinitionEqual(existing, kind) {
			continue
		}
		if err := m.srv.objects.compile(kind); err != nil {
			return fmt.Errorf("built-in kind %s does not compile: %v", kind.KindRef(), err)
		}
		req := &structs.ObjectKindUpsertRequest{
			ObjectKind:   kind,
			WriteRequest: structs.WriteRequest{Region: m.srv.Region()},
		}
		if _, _, err := m.srv.raftApply(structs.ObjectKindUpsertRequestType|structs.IgnoreUnknownTypeFlag, req); err != nil {
			return err
		}
		m.srv.objects.forget(kind.KindRef())
	}

	m.lock.Lock()
	m.kindsReady = true
	m.lock.Unlock()
	return nil
}

// resetKinds makes the next ensureKinds check the kinds again, e.g. after a
// leadership change or a snapshot restore.
func (m *releaseManager) resetKinds() {
	m.lock.Lock()
	m.kindsReady = false
	m.lock.Unlock()
}

func objectKindDefinitionEqual(a, b *structs.ObjectKind) bool {
	return a.Description == b.Description &&
		a.Namespaced == b.Namespaced &&
		a.Schema == b.Schema &&
		reflect.DeepEqual(a.Rules, b.Rules) &&
		reflect.DeepEqual(a.Columns, b.Columns)
}

// releaseLeaderLoop runs on the leader: it marks operations interrupted by
// a leader change as failed and checks the releases for drift periodically.
// The built-in kinds are registered by the first release operation, so that
// clusters without releases don't list them.
func (s *Server) releaseLeaderLoop(stopCh chan struct{}) {
	m := s.releases
	m.resetKinds()
	m.failInterrupted()

	// Spread the drift checks of several leaders.
	timer := time.NewTimer(releaseDriftInterval/2 + rand.N(releaseDriftInterval/2))
	defer timer.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-timer.C:
		}
		m.checkAllDrift()
		timer.Reset(releaseDriftInterval)
	}
}

// failInterrupted marks releases whose operation was interrupted by a
// leader change as failed, so that they can be operated on again.
func (m *releaseManager) failInterrupted() {
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return
	}
	iter, err := snap.CustomObjects(nil, structs.ReleaseObjectGroup, structs.ReleaseKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		rel, err := structs.ReleaseFromObject(obj)
		if err != nil {
			continue
		}
		switch rel.Status.Phase {
		case structs.ReleasePhaseApplying, structs.ReleasePhaseRollingBack, structs.ReleasePhaseUninstalling:
		default:
			continue
		}
		if m.isBusy(rel.Namespace, rel.Name) {
			continue
		}
		rel.Status.Phase = structs.ReleasePhaseFailed
		if rel.Status.LastOperation != nil {
			rel.Status.LastOperation.Error = "the operation was interrupted by a leader change"
			rel.Status.LastOperation.EndTime = time.Now().UTC().Format(time.RFC3339)
		}
		if _, err := m.writeRelease(rel, obj.ModifyIndex); err != nil {
			m.logger.Warn("failed to mark interrupted release operation as failed",
				"namespace", rel.Namespace, "release", rel.Name, "error", err)
		}
	}
}

// writeRelease writes a release object with check-and-set on expected (0:
// the release must not exist). It returns the written release.
func (m *releaseManager) writeRelease(rel *structs.Release, expected uint64) (*structs.Release, error) {
	spec, status, err := rel.ObjectParts()
	if err != nil {
		return nil, err
	}
	obj := &structs.CustomObject{
		ID:         rel.ID,
		Group:      structs.ReleaseObjectGroup,
		Kind:       structs.ReleaseKind,
		Namespace:  rel.Namespace,
		Name:       rel.Name,
		ManagedBy:  structs.ReleaseManagedBy,
		Spec:       spec,
		Status:     status,
		CreateTime: rel.CreateTime,
	}
	if obj.ID == "" {
		obj.ID = uuid.Generate()
	}
	written, err := m.writeObject(obj, expected)
	if err != nil {
		return nil, err
	}
	return structs.ReleaseFromObject(written)
}

// writeRevision writes a revision object.
func (m *releaseManager) writeRevision(rev *structs.ReleaseRevision, owner *structs.Release, expected uint64) (*structs.ReleaseRevision, error) {
	spec, status, err := rev.ObjectParts()
	if err != nil {
		return nil, err
	}
	obj := &structs.CustomObject{
		ID:        uuid.Generate(),
		Group:     structs.ReleaseObjectGroup,
		Kind:      structs.ReleaseRevisionKind,
		Namespace: rev.Namespace,
		Name:      structs.ReleaseRevisionObjectName(rev.Release, rev.Revision),
		Labels:    map[string]string{structs.ReleaseLabel: rev.Release},
		ManagedBy: structs.ReleaseManagedBy,
		Owners: []*structs.ObjectOwnerReference{{
			Kind:      structs.ReleaseKindRef,
			Namespace: owner.Namespace,
			Name:      owner.Name,
			ID:        owner.ID,
		}},
		Spec:   spec,
		Status: status,
	}
	written, err := m.writeObject(obj, expected)
	if err != nil {
		return nil, err
	}
	out, err := structs.ReleaseRevisionFromObject(written)
	if err != nil {
		return nil, err
	}
	out.Resources = rev.Resources
	return out, nil
}

// writeObject validates an object of a built-in kind against its schema and
// rules and writes it with check-and-set on expected. The caller has
// authorized the write.
func (m *releaseManager) writeObject(obj *structs.CustomObject, expected uint64) (*structs.CustomObject, error) {
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return nil, err
	}
	kind, err := snap.ObjectKindByName(nil, obj.Group, obj.Kind)
	if err != nil {
		return nil, err
	}
	if kind == nil {
		return nil, fmt.Errorf("built-in object kind %s is not registered yet", obj.KindRef())
	}
	existing, err := snap.CustomObjectByName(nil, obj.Group, obj.Kind, obj.Namespace, obj.Name)
	if err != nil {
		return nil, err
	}
	switch {
	case existing == nil && expected != 0:
		return nil, structs.NewObjectConflictError("%s %q does not exist", obj.KindRef(), obj.Name)
	case existing != nil && existing.ModifyIndex != expected:
		return nil, structs.NewObjectConflictError("%s %q has modify index %d, expected %d",
			obj.KindRef(), obj.Name, existing.ModifyIndex, expected)
	}

	now := time.Now().UnixNano()
	if obj.Spec, err = structs.NormalizeObjectMap(obj.Spec); err != nil {
		return nil, err
	}
	if obj.Status, err = structs.NormalizeObjectMap(obj.Status); err != nil {
		return nil, err
	}
	obj.ModifyTime = now
	obj.Generation = 1
	if obj.CreateTime == 0 {
		obj.CreateTime = now
	}
	operation := objectOperationCreate
	if existing != nil {
		operation = objectOperationUpdate
		obj.ID = existing.ID
		obj.CreateTime = existing.CreateTime
		obj.CreateIndex = existing.CreateIndex
		obj.ModifyIndex = existing.ModifyIndex
		obj.Generation = existing.Generation
		if !structs.ObjectMapsEqual(existing.Spec, obj.Spec) {
			obj.Generation++
		}
		obj.Finalizers = existing.Finalizers
		obj.DeletionTime = existing.DeletionTime
	}

	co := &CustomObject{srv: m.srv}
	w := &objectWrite{args: &structs.GenericRequest{}}
	if err := co.validateObject(kind, obj, existing, operation, w); err != nil {
		return nil, err
	}

	req := &structs.CustomObjectUpsertStateRequest{
		Object:              obj,
		ExpectedModifyIndex: expected,
		WriteRequest:        structs.WriteRequest{Region: m.srv.Region()},
	}
	_, index, err := m.srv.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	if err != nil {
		return nil, err
	}
	obj.ModifyIndex = index
	if existing == nil {
		obj.CreateIndex = index
	}
	return obj, nil
}

// deleteObject deletes an object of a built-in kind.
func (m *releaseManager) deleteObject(kind, namespace, name string) error {
	req := &structs.CustomObjectDeleteStateRequest{
		Group:        structs.ReleaseObjectGroup,
		Kind:         kind,
		Namespace:    namespace,
		Name:         name,
		WriteRequest: structs.WriteRequest{Region: m.srv.Region()},
	}
	_, _, err := m.srv.raftApply(structs.CustomObjectDeleteRequestType|structs.IgnoreUnknownTypeFlag, req)
	return err
}

// releaseObject returns the object and the typed view of a release, or nil.
func releaseObject(snap *state.StateSnapshot, namespace, name string) (*structs.CustomObject, *structs.Release, error) {
	obj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.ReleaseKind, namespace, name)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	rel, err := structs.ReleaseFromObject(obj)
	if err != nil {
		return nil, nil, err
	}
	return obj, rel, nil
}

// releaseRevisions returns the revisions of a release, oldest first.
func releaseRevisions(snap *state.StateSnapshot, namespace, name string) ([]*structs.ReleaseRevision, error) {
	iter, err := snap.CustomObjectsByLabel(nil, structs.ReleaseLabel, name, state.SortDefault)
	if err != nil {
		return nil, err
	}
	var out []*structs.ReleaseRevision
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		if obj.Group != structs.ReleaseObjectGroup || obj.Kind != structs.ReleaseRevisionKind || obj.Namespace != namespace {
			continue
		}
		rev, err := structs.ReleaseRevisionFromObject(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	sortRevisions(out)
	return out, nil
}

func sortRevisions(revs []*structs.ReleaseRevision) {
	for i := 1; i < len(revs); i++ {
		for j := i; j > 0 && revs[j].Revision < revs[j-1].Revision; j-- {
			revs[j], revs[j-1] = revs[j-1], revs[j]
		}
	}
}

// releaseRevision returns a revision with its decoded resources, or nil.
func releaseRevision(snap *state.StateSnapshot, namespace, name string, revision uint64) (*structs.ReleaseRevision, uint64, error) {
	obj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.ReleaseRevisionKind,
		namespace, structs.ReleaseRevisionObjectName(name, revision))
	if err != nil || obj == nil {
		return nil, 0, err
	}
	rev, err := structs.ReleaseRevisionFromObject(obj)
	if err != nil {
		return nil, 0, err
	}
	if rev.Resources, err = structs.DecodeReleaseResources(rev.Spec.Content); err != nil {
		return nil, 0, err
	}
	return rev, obj.ModifyIndex, nil
}

// errBuiltinObjectGroup is the error of generic object API writes to the
// built-in group.
func errBuiltinObjectGroup(group string) error {
	return structs.NewErrRPCCodedf(http.StatusBadRequest,
		"objects and kinds of the group %q are built in and managed by the release API", group)
}

// jsonEqual reports whether two values have the same JSON encoding.
func jsonEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ab) == string(bb)
}

// recordEvent writes an event to the object event log of a release, so
// that operations, test results and drift show up next to the release.
// It is best effort.
func (m *releaseManager) recordEvent(namespace, name, eventType, reason, message string, meta map[string]string) {
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return
	}
	obj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.ReleaseKind, namespace, name)
	if err != nil || obj == nil {
		return
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	ev := &structs.ObjectEvent{Type: eventType, Reason: reason, Message: message, Meta: meta}
	if _, err := m.srv.recordObjectEvents(obj, []*structs.ObjectEvent{ev}, structs.ReleaseManagedBy); err != nil {
		m.logger.Debug("failed to record release event", "release", name, "error", err)
	}
}
