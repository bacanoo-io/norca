// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"slices"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/objectgc"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// setupObjectGC creates the garbage collector of custom objects. It runs only
// on the leader (see establishLeadership).
func (s *Server) setupObjectGC() {
	s.objectGC = objectgc.New(s.logger, objectgc.DefaultConfig(), &objectgc.Deps{
		State:            s.State,
		DeleteObject:     s.gcDeleteObject,
		Guarded:          objectDeletionDestroys,
		ReleaseObject:    s.gcReleaseObject,
		RemoveObject:     s.gcRemoveObject,
		StopJob:          s.gcStopJob,
		KeepJob:          s.gcKeepJob,
		DeleteTombstones: s.gcDeleteTombstones,
		ServersReady: func() bool {
			return ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false)
		},
	})
}

// gcDeleteObject deletes a dependent object in the background mode: it is
// marked for deletion if it has finalizers, and removed otherwise. approved
// is recorded with the deletion (CustomObject.DeletionApproved).
func (s *Server) gcDeleteObject(obj *structs.CustomObject, approved []string) error {
	if len(obj.Finalizers) == 0 {
		_, err := s.removeCustomObjectApproved(obj, structs.ObjectPropagationBackground, approved)
		return err
	}
	marked := obj.Copy()
	marked.DeletionTime = time.Now().UnixNano()
	marked.DeletionPropagation = structs.ObjectPropagationBackground
	marked.DeletionApproved = slices.Clone(approved)
	marked.ModifyTime = marked.DeletionTime
	req := &structs.CustomObjectUpsertStateRequest{
		Object:              marked,
		ExpectedModifyIndex: obj.ModifyIndex,
		WriteRequest:        structs.WriteRequest{Region: s.Region()},
	}
	_, _, err := s.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	return err
}

// gcReleaseObject lets go of a dependent that is not deleted with its owner:
// its deletion destroys data in the storage, and nobody who deleted the
// owner was checked for the right to do that. The references to the owner
// are removed, so the object stays as one without that owner, and an event
// says why.
func (s *Server) gcReleaseObject(obj *structs.CustomObject, ownerKindRef, ownerNamespace, ownerName string) error {
	released := obj.Copy()
	released.Owners = slices.DeleteFunc(released.Owners, func(r *structs.ObjectOwnerReference) bool {
		return r != nil && r.Kind == ownerKindRef && r.Namespace == ownerNamespace && r.Name == ownerName
	})
	released.ModifyTime = time.Now().UnixNano()
	req := &structs.CustomObjectUpsertStateRequest{
		Object:              released,
		ExpectedModifyIndex: obj.ModifyIndex,
		WriteRequest:        structs.WriteRequest{Region: s.Region()},
	}
	if _, _, err := s.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, req); err != nil {
		return err
	}
	s.operatorEvent(released, structs.ObjectEventTypeWarning, objectEventDependentKept, fmt.Sprintf(
		"the owner %s %q was deleted, and this object was not deleted with it: its deletion deletes data in the storage "+
			"(its volume or its snapshots), and the deletion of the owner was not checked for the right to do that for this object. "+
			"Its reference to the owner was removed; delete the object itself if it is to go.", ownerKindRef, ownerName))
	return nil
}

// objectEventDependentKept is the reason of the event of gcReleaseObject.
const objectEventDependentKept = "DependentKept"

func (s *Server) gcRemoveObject(obj *structs.CustomObject, propagation string) error {
	_, err := s.removeCustomObject(obj, propagation)
	return err
}

// gcStopJob stops a dependent job through Job.Deregister with the leader's
// token, which also passes the protection of managed jobs.
func (s *Server) gcStopJob(namespace, id string) error {
	args := &structs.JobDeregisterRequest{
		JobID: id,
		Purge: false,
		WriteRequest: structs.WriteRequest{
			Region:    s.Region(),
			Namespace: namespace,
			AuthToken: s.getLeaderAcl(),
		},
	}
	var reply structs.JobDeregisterResponse
	return s.RPC("Job.Deregister", args, &reply)
}

// gcKeepJob records that a dependent job was not stopped with its owner:
// nobody who deleted the owner was checked for the right to stop it. A job
// has no event log, so the event is recorded for the owner while that still
// exists (an owner that waits for its dependents in the foreground mode);
// the collector logs it either way.
func (s *Server) gcKeepJob(job *structs.Job, ownerKindRef, ownerNamespace, ownerName string) {
	group, kind, err := structs.ParseObjectKindRef(ownerKindRef)
	if err != nil {
		return
	}
	owner, err := s.State().CustomObjectByName(nil, group, kind, ownerNamespace, ownerName)
	if err != nil || owner == nil {
		return
	}
	s.operatorEvent(owner, structs.ObjectEventTypeWarning, objectEventDependentKept, fmt.Sprintf(
		"the job %q in the namespace %q depends on this object and is not stopped with it: "+
			"the deletion of this object was not checked for the right to stop that job. Stop the job itself if it is to go.",
		job.ID, job.Namespace))
}

func (s *Server) gcDeleteTombstones(ids []string) error {
	req := &structs.ObjectGCRequest{
		DeleteTombstones: ids,
		WriteRequest:     structs.WriteRequest{Region: s.Region()},
	}
	_, _, err := s.raftApply(structs.ObjectGCRequestType|structs.IgnoreUnknownTypeFlag, req)
	return err
}
