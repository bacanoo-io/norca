// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

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
// to have the volume or the snapshots deleted. The same holds for a tenant
// and a certificate (review finding JOG-22): the servers take down what the
// tenant keeps, and stop keeping the certificate, so an owner of one is
// deleted with it only by a caller who may delete the tenant or the
// certificate itself. And for a node group and a certificate issuer (review
// finding JOG-26): the deletion of a node group is for the machines of the
// group, which its operator (outside this tree) then acts on as the deletion
// policy of the group says, and with an issuer gone the servers issue and
// renew nothing for the certificates that name it (certificateOperator
// refusal, IssuerNotFound). The collector also stops the jobs that depend on
// a deleted object, which needs the right to stop a job in the same way.
//
// Not guarded on purpose (decision of the maintainer with JOG-26): the kinds
// of operators outside this tree whose objects are typically the dependents
// of an application, norca.io/LoadBalancer and norca.io/DNSRecord. They go
// with the application they were made for, by whoever may delete that.

// deletionGuard is what the deletion of an object takes with it beyond the
// object, and the right the deletion needs for that, besides the right to
// write the kind where the object is deleted itself.
type deletionGuard struct {
	// unnamed is the object as a dependent, to a caller who may not read
	// it.
	unnamed string

	// takes is what the servers delete, or let go of, with the object.
	takes string

	// needs is the right as an error names it; "that namespace" is the
	// namespace of the object.
	needs string

	// allowed reports whether the ACL has that right for the object.
	allowed func(aclObj *acl.ACL, obj *structs.CustomObject) bool
}

// allowVolumeDeletion is the right of the volume API to delete volumes and
// snapshots of the namespace of the object (checkVolumeRequestDelete,
// checkSnapshotDelete).
func allowVolumeDeletion(aclObj *acl.ACL, obj *structs.CustomObject) bool {
	return acl.NamespaceValidator(acl.NamespaceCapabilityCSIWriteVolume)(aclObj, obj.Namespace) && aclObj.AllowPluginRead()
}

// allowObjectDeletion is the right to delete the object itself through the
// object API: "write" on its kind, in its namespace or cluster-scoped.
func allowObjectDeletion(aclObj *acl.ACL, obj *structs.CustomObject) bool {
	return aclObj.AllowObjectOperation(objectOwnerACLNamespace(obj.Namespace), obj.KindRef(), acl.ObjectCapabilityWrite)
}

var (
	storageDeletionGuard = &deletionGuard{
		unnamed: "a volume request or a snapshot schedule that depends on it",
		takes:   "data in the storage (its volume or its snapshots)",
		needs:   "the right " + acl.NamespaceCapabilityCSIWriteVolume + " in that namespace and to read CSI plugins",
		allowed: allowVolumeDeletion,
	}
	tenantDeletionGuard = &deletionGuard{
		unnamed: "a tenant that depends on it",
		takes:   "what the tenant keeps (its namespace, quota, ACL policies and roles, and network rule)",
		needs: "the object capability " + acl.ObjectCapabilityWrite + " on the kind " +
			structs.OperatorsGroup + "/" + structs.TenantKind,
		allowed: allowObjectDeletion,
	}
	certificateDeletionGuard = &deletionGuard{
		unnamed: "a certificate that depends on it",
		takes:   "the certificate the servers keep issued (its renewal, its valid certificates, which are revoked unless keep_certificates is set, and its secret with delete_secret)",
		needs: "the object capability " + acl.ObjectCapabilityWrite + " on the kind " +
			structs.OperatorsGroup + "/" + structs.CertificateKind + " in that namespace",
		allowed: allowObjectDeletion,
	}
	// What the operator of the group does with the machines is not in this
	// tree: the text says what the kind says (spec.deletion.policy of
	// nodeGroupKindSchema).
	nodeGroupDeletionGuard = &deletionGuard{
		unnamed: "a node group that depends on it",
		takes:   "the nodes of the group (its operator acts on the machines of the group as the deletion policy of the group says: drain, the default, terminate or orphan)",
		needs: "the object capability " + acl.ObjectCapabilityWrite + " on the kind " +
			structs.NodeScalingGroup + "/" + structs.NodeGroupKind,
		allowed: allowObjectDeletion,
	}
	// The issuing CA of the region is not deleted with the last built-in
	// issuer: it stays and is still rotated, only none is created where
	// there is none (certificateCA.reconcile).
	certificateIssuerDeletionGuard = &deletionGuard{
		unnamed: "a certificate issuer that depends on it",
		takes:   "the issuing of the certificates that name it (nothing is issued or renewed for them any more; what they have stays until it expires)",
		needs: "the object capability " + acl.ObjectCapabilityWrite + " on the kind " +
			structs.OperatorsGroup + "/" + structs.CertificateIssuerKind,
		allowed: allowObjectDeletion,
	}
)

// objectDeletionGuard returns what the deletion of the object takes with it
// beyond the object, or nil for an object whose deletion removes the object
// and nothing else. Of the kinds of certificates the certificate and the
// issuer are guarded: an order is the servers'. Of the kinds of node scaling
// the node group is, the node provider is not (the decision names the group).
func objectDeletionGuard(obj *structs.CustomObject) *deletionGuard {
	switch {
	case isVolumeRequestKind(obj.Group, obj.Kind):
		if volumeRequestDeletionDestroys(obj) {
			return storageDeletionGuard
		}
	case isSnapshotKind(obj.Group, obj.Kind):
		if snapshotDeletionDestroys(obj) {
			return storageDeletionGuard
		}
	case isTenantKind(obj.Group, obj.Kind):
		return tenantDeletionGuard
	case obj.Group == structs.OperatorsGroup && obj.Kind == structs.CertificateKind:
		return certificateDeletionGuard
	case obj.Group == structs.OperatorsGroup && obj.Kind == structs.CertificateIssuerKind:
		return certificateIssuerDeletionGuard
	case obj.Group == structs.NodeScalingGroup && obj.Kind == structs.NodeGroupKind:
		return nodeGroupDeletionGuard
	}
	return nil
}

// objectDeletionDestroys reports whether the deletion of the object makes the
// servers delete, or let go of, something beyond the object: data in the
// storage, the resources of a tenant, a certificate, the certificates of an
// issuer, the nodes of a node group.
func objectDeletionDestroys(obj *structs.CustomObject) bool {
	return objectDeletionGuard(obj) != nil
}

// objectRefKey is the key of an object or of a reference to it.
func objectRefKey(kindRef, namespace, name string) string {
	return kindRef + "\x00" + namespace + "\x00" + name
}

// objectDependents returns the dependents of owner, directly or through
// other objects, sorted by kind, namespace and name. It follows every owner
// reference: the garbage collector deletes a dependent only when all its
// owners are gone, but two deletions that run at the same time don't see
// each other, so every owner of a dependent counts as the one whose deletion
// takes it. The owner index of the state store finds them, without a scan of
// all objects (review finding MUK-9).
func objectDependents(snap *state.StateSnapshot, owner *structs.CustomObject) ([]*structs.CustomObject, error) {
	seen := map[string]struct{}{objectRefKey(owner.KindRef(), owner.Namespace, owner.Name): {}}
	var out []*structs.CustomObject
	for queue := []*structs.CustomObject{owner}; len(queue) > 0; {
		up := queue[0]
		queue = queue[1:]
		iter, err := snap.CustomObjectsByOwner(nil, up.KindRef(), up.Namespace, up.Name)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			dep := raw.(*structs.CustomObject)
			key := objectRefKey(dep.KindRef(), dep.Namespace, dep.Name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, dep)
			queue = append(queue, dep)
		}
	}
	slices.SortFunc(out, func(a, b *structs.CustomObject) int {
		return strings.Compare(objectRefKey(a.KindRef(), a.Namespace, a.Name), objectRefKey(b.KindRef(), b.Namespace, b.Name))
	})
	return out, nil
}

// destroyingDependents returns the dependents of owner, direct or through
// other objects, whose deletion takes more than the object with it
// (objectDeletionDestroys). deps are the dependents of owner
// (objectDependents).
func destroyingDependents(deps []*structs.CustomObject) []*structs.CustomObject {
	var out []*structs.CustomObject
	for _, dep := range deps {
		if objectDeletionDestroys(dep) {
			out = append(out, dep)
		}
	}
	return out
}

// dependentJobs returns the running jobs that the deletion of owner may
// stop: the jobs whose meta names owner, or an object that depends on owner,
// sorted by namespace and ID. deps are the dependents of owner
// (objectDependents). A job that is stopped already is not one of them:
// nothing is done to it.
func dependentJobs(snap *state.StateSnapshot, owner *structs.CustomObject, deps []*structs.CustomObject) ([]*structs.Job, error) {
	seen := map[structs.NamespacedID]struct{}{}
	var out []*structs.Job
	for _, obj := range append([]*structs.CustomObject{owner}, deps...) {
		iter, err := snap.JobsByOwner(nil, structs.ObjectOwnerMetaValue(obj.Group, obj.Kind, obj.Namespace, obj.Name))
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			job := raw.(*structs.Job)
			if job.Stop || !structs.JobOwnedBy(job.Meta, obj.Group, obj.Kind, obj.Namespace, obj.Name) {
				continue
			}
			id := job.NamespacedID()
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, job)
		}
	}
	slices.SortFunc(out, func(a, b *structs.Job) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// checkObjectDependentsDelete checks the deletion of an object that takes
// its dependents with it (the background and the foreground mode). The
// caller needs, for every dependent object whose deletion takes more than
// the object with it (objectDeletionGuard: data in the storage, the resources
// of a tenant, a certificate), what deleting that dependent itself needs for
// that: the right of the guard, and, for a dependent that is managed
// (managed_by), to be its manager (checkObjectManagedBy; review finding
// JOG-25): the collector deletes the dependent with the rights of the
// servers, so otherwise an owner would be the way around the protection that
// a direct deletion of the dependent meets. For every dependent job the
// caller needs the right to stop it (submit-job in its namespace, as
// Job.Deregister asks). The protection of a managed job (norca.managed-by)
// is not asked for: it keeps others from changing the job behind its
// manager, and the object that owns the job is the way they are meant to
// take. args is the request of the caller, for its identity. It returns the
// list of the dependents the caller was checked for, as it is recorded with
// the deletion (CustomObject.DeletionApproved).
func checkObjectDependentsDelete(snap *state.StateSnapshot, aclObj *acl.ACL, args structs.RequestWithIdentity, owner *structs.CustomObject) ([]string, error) {
	var approved []string

	dependents, err := objectDependents(snap, owner)
	if err != nil {
		return nil, err
	}
	jobs, err := dependentJobs(snap, owner, dependents)
	if err != nil {
		return nil, err
	}
	allowStop := acl.NamespaceValidator(acl.NamespaceCapabilitySubmitJob)
	for _, job := range jobs {
		if allowStop(aclObj, job.Namespace) {
			approved = append(approved, structs.ObjectApprovedJob(job.Namespace, job.ID, job.CreateIndex))
			continue
		}
		// The job is named only to a caller who may read it.
		which := "a job that depends on it"
		if aclObj.AllowNsOp(job.Namespace, acl.NamespaceCapabilityReadJob) {
			which = "its dependent job \"" + job.ID + "\""
		}
		return nil, structs.NewErrRPCCodedf(http.StatusForbidden,
			"deleting %s %q would stop %s in the namespace %q: that needs the right %s in that namespace; "+
				"the propagation mode %q deletes the object and keeps its dependents",
			owner.KindRef(), owner.Name, which, job.Namespace, acl.NamespaceCapabilitySubmitJob, structs.ObjectPropagationOrphan)
	}

	deps := destroyingDependents(dependents)
	for _, dep := range deps {
		guard := objectDeletionGuard(dep)
		// The dependent, and its manager, are named only to a caller who
		// may read it.
		readable := aclObj.AllowObjectOperation(objectOwnerACLNamespace(dep.Namespace), dep.KindRef(), acl.ObjectCapabilityRead)
		var refused string
		switch {
		case !guard.allowed(aclObj, dep):
			refused = "that needs " + guard.needs
		case checkObjectManagedBy(aclObj, args, dep) != nil:
			refused = "it is managed by another identity, and only that identity may delete it"
			if readable {
				refused = fmt.Sprintf("it is managed by %q, and only that identity may delete it", dep.ManagedBy)
			}
		default:
			continue
		}
		which := guard.unnamed
		if readable {
			which = "its dependent " + dep.KindRef() + " \"" + dep.Name + "\""
		}
		where := ""
		if dep.Namespace != "" {
			where = fmt.Sprintf(" in the namespace %q", dep.Namespace)
		}
		return nil, structs.NewErrRPCCodedf(http.StatusForbidden,
			"deleting %s %q would delete %s%s, and with it %s: %s; "+
				"the propagation mode %q deletes the object and keeps its dependents",
			owner.KindRef(), owner.Name, which, where, guard.takes, refused, structs.ObjectPropagationOrphan)
	}
	for _, dep := range deps {
		approved = append(approved, dep.ID)
	}
	slices.Sort(approved)
	return approved, nil
}
