// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The operator of snapshot schedules acts with the rights of the servers, so
// what a writer of a schedule may cause is checked when it is written: the
// writer must be allowed to take and delete snapshots of volumes of the
// namespace itself (csi-write-volume, as the snapshot API asks), and to read
// the secret the CSI secrets come from.

// checkSnapshotApply checks a snapshot schedule written through the object
// API. obj has the defaults of its schema.
func checkSnapshotApply(aclObj *acl.ACL, obj *structs.CustomObject) error {
	spec := &snapshotScheduleSpec{}
	if err := structs.DecodeObjectPart(obj.Spec, spec); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid snapshot schedule: %v", err)
	}
	if _, _, _, err := spec.parse(); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid snapshot schedule: %v", err)
	}

	allowVolume := acl.NamespaceValidator(acl.NamespaceCapabilityCSIWriteVolume)
	if !allowVolume(aclObj, obj.Namespace) || !aclObj.AllowPluginRead() {
		return structs.NewErrRPCCodedf(http.StatusForbidden,
			"a snapshot schedule needs the right %s in the namespace %q and to read CSI plugins",
			acl.NamespaceCapabilityCSIWriteVolume, obj.Namespace)
	}
	if spec.SecretPath != "" && !aclObj.AllowSecretOperation(obj.Namespace, spec.SecretPath, acl.NamespaceCapabilityReadSecret) {
		return structs.NewErrRPCCodedf(http.StatusForbidden,
			"a snapshot schedule needs the right to read its secret %q in the namespace %q", spec.SecretPath, obj.Namespace)
	}
	return nil
}

// checkSnapshotStatusWrite refuses status writes through the object API: the
// status of a schedule is the servers'.
func checkSnapshotStatusWrite() error {
	return structs.NewErrRPCCodedf(http.StatusForbidden, "the status of a snapshot schedule is written by the servers only")
}
