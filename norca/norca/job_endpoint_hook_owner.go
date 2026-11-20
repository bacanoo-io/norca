// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"net/http"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// jobReservedMetaKeys are the meta keys of a job that the servers act on:
// the owner mark (the deletion of the owner stops the job, and asks the
// deleter for the right to stop it) and the identity that manages the job
// (nobody else changes it). A registration sets them, with the checks of
// checkJobOwnerMark and checkManagedJob; a dispatch does not.
var jobReservedMetaKeys = []string{
	structs.JobMetaOwner,
	structs.JobMetaOwnerKind,
	structs.JobMetaManagedBy,
}

// checkJobOwnerMark refuses a registration that marks a job as the dependent
// of an object (the meta structs.JobMetaOwner, with structs.JobMetaOwnerKind)
// unless the caller has on the owner what naming an owner of an object takes
// (allowObjectOwner: "write" or "manage-finalizers" on the kind of the owner
// in the namespace of the owner; review finding JOG-20). The garbage
// collector stops the dependent jobs of a deleted object, and the deletion
// of the object is refused to a caller who may not stop them
// (checkObjectDependentsDelete), so the right to submit jobs in one
// namespace must not be enough to make a job the dependent of any object.
//
// The mark is checked when it is new: on a new job, and when the owner or
// its kind differs from that of the stored job. A registration that keeps
// the mark of the stored job is not checked again, so whoever may update the
// job still can, and removing the mark takes no right on the owner. The
// owner need not exist, and nothing is read to decide: the answer is the
// same for an owner that exists and for one that does not. A value that
// names no owner is refused, since no right can be checked for it.
//
// Management tokens (the leader's included) and clusters without ACLs are
// not restricted. The children of periodic and parameterized jobs inherit
// the meta of their parent and are written through Raft, not through
// Job.Register (see checkJobParent): the mark of the parent was checked when
// the parent was registered, and a dispatch can't set one
// (checkDispatchReservedMeta).
func checkJobOwnerMark(aclObj *acl.ACL, existing, job *structs.Job) error {
	if aclObj == nil || aclObj.IsManagement() {
		return nil
	}
	value, ok := job.Meta[structs.JobMetaOwner]
	if !ok {
		return nil
	}
	kindMeta, hasKindMeta := job.Meta[structs.JobMetaOwnerKind]
	if existing != nil {
		prev, had := existing.Meta[structs.JobMetaOwner]
		prevKind, hadKind := existing.Meta[structs.JobMetaOwnerKind]
		if had && prev == value && hadKind == hasKindMeta && prevKind == kindMeta {
			return nil
		}
	}

	group, kind, namespace, name, err := structs.ParseObjectOwnerMetaValue(value)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"job %q: invalid owner in the meta %q: %v", job.ID, structs.JobMetaOwner, err)
	}
	kindRef := structs.ObjectKindRef(group, kind)
	if hasKindMeta && kindMeta != kindRef {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"job %q: the meta %q is %q, but the meta %q names an owner of the kind %q",
			job.ID, structs.JobMetaOwnerKind, kindMeta, structs.JobMetaOwner, kindRef)
	}
	if !allowObjectOwner(aclObj, kindRef, namespace) {
		return errObjectOwnerAccess(kindRef, namespace, name)
	}
	return nil
}

// isJobReservedMetaKey reports whether the meta key is one the servers act
// on (jobReservedMetaKeys).
func isJobReservedMetaKey(key string) bool {
	for _, reserved := range jobReservedMetaKeys {
		if key == reserved {
			return true
		}
	}
	return false
}

// checkDispatchReservedMeta refuses a dispatch that sets a reserved meta key
// (jobReservedMetaKeys), whether the parameterized job lists the key or not:
// the child of a dispatch is written without the checks of a registration,
// so with the right to dispatch a job alone a caller would mark the child as
// the dependent of any object, or as managed by any identity. The child
// keeps the reserved meta of its parent.
func checkDispatchReservedMeta(meta map[string]string) error {
	for _, key := range jobReservedMetaKeys {
		if _, ok := meta[key]; ok {
			return structs.NewErrRPCCodedf(http.StatusBadRequest,
				"the meta key %q is reserved: a dispatch can't set it", key)
		}
	}
	return nil
}

// jobReservedMetaHook refuses a parameterized job that lists a reserved meta
// key (jobReservedMetaKeys) among the keys a dispatch sets: the dispatch
// would be refused (checkDispatchReservedMeta), so the job says something it
// can't do.
type jobReservedMetaHook struct{}

func (jobReservedMetaHook) Name() string {
	return "reserved-meta"
}

func (jobReservedMetaHook) Validate(job *structs.Job) ([]error, error) {
	if job.ParameterizedJob == nil {
		return nil, nil
	}
	for _, key := range job.ParameterizedJob.MetaRequired {
		if isJobReservedMetaKey(key) {
			return nil, fmt.Errorf("parameterized job: the meta key %q is reserved and can't be listed in meta_required: a dispatch can't set it", key)
		}
	}
	for _, key := range job.ParameterizedJob.MetaOptional {
		if isJobReservedMetaKey(key) {
			return nil, fmt.Errorf("parameterized job: the meta key %q is reserved and can't be listed in meta_optional: a dispatch can't set it", key)
		}
	}
	return nil, nil
}
