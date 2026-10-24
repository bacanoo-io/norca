// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"time"

	"github.com/hashicorp/nomad/nomad/objectgc"
	"github.com/hashicorp/nomad/nomad/structs"
)

// setupObjectGC creates the garbage collector of custom objects. It runs only
// on the leader (see establishLeadership).
func (s *Server) setupObjectGC() {
	s.objectGC = objectgc.New(s.logger, objectgc.DefaultConfig(), &objectgc.Deps{
		State:            s.State,
		DeleteObject:     s.gcDeleteObject,
		RemoveObject:     s.gcRemoveObject,
		StopJob:          s.gcStopJob,
		DeleteTombstones: s.gcDeleteTombstones,
		ServersReady: func() bool {
			return ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false)
		},
	})
}

// gcDeleteObject deletes a dependent object in the background mode: it is
// marked for deletion if it has finalizers, and removed otherwise.
func (s *Server) gcDeleteObject(obj *structs.CustomObject) error {
	if len(obj.Finalizers) == 0 {
		_, err := s.removeCustomObject(obj, structs.ObjectPropagationBackground)
		return err
	}
	marked := obj.Copy()
	marked.DeletionTime = time.Now().UnixNano()
	marked.DeletionPropagation = structs.ObjectPropagationBackground
	marked.ModifyTime = marked.DeletionTime
	req := &structs.CustomObjectUpsertStateRequest{
		Object:              marked,
		ExpectedModifyIndex: obj.ModifyIndex,
		WriteRequest:        structs.WriteRequest{Region: s.Region()},
	}
	_, _, err := s.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	return err
}

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

func (s *Server) gcDeleteTombstones(ids []string) error {
	req := &structs.ObjectGCRequest{
		DeleteTombstones: ids,
		WriteRequest:     structs.WriteRequest{Region: s.Region()},
	}
	_, _, err := s.raftApply(structs.ObjectGCRequestType|structs.IgnoreUnknownTypeFlag, req)
	return err
}
