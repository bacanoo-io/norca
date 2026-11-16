// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The operator of volume requests acts with the rights of the servers, so
// what a writer of a request may cause is checked when it is written: the
// writer must be allowed to create, register and delete volumes of the
// namespace itself (csi-write-volume and plugin read, as the volume API
// asks), and to read the secret the CSI secrets come from.

// checkVolumeRequestApply checks a volume request written through the object
// API. obj has the defaults of its schema.
func checkVolumeRequestApply(aclObj *acl.ACL, obj *structs.CustomObject) error {
	spec := &volumeRequestSpec{}
	if err := structs.DecodeObjectPart(obj.Spec, spec); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid volume request: %v", err)
	}
	if err := spec.validate(); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid volume request: %v", err)
	}

	allowVolume := acl.NamespaceValidator(acl.NamespaceCapabilityCSIWriteVolume)
	if !allowVolume(aclObj, obj.Namespace) || !aclObj.AllowPluginRead() {
		return structs.NewErrRPCCodedf(http.StatusForbidden,
			"a volume request needs the right %s in the namespace %q and to read CSI plugins",
			acl.NamespaceCapabilityCSIWriteVolume, obj.Namespace)
	}
	if spec.SecretPath != "" && !aclObj.AllowSecretOperation(obj.Namespace, spec.SecretPath, acl.NamespaceCapabilityReadSecret) {
		return structs.NewErrRPCCodedf(http.StatusForbidden,
			"a volume request needs the right to read its secret %q in the namespace %q", spec.SecretPath, obj.Namespace)
	}
	return nil
}

// checkVolumeRequestStatusWrite refuses status writes through the object
// API: the status of a request is the servers'.
func checkVolumeRequestStatusWrite() error {
	return structs.NewErrRPCCodedf(http.StatusForbidden, "the status of a volume request is written by the servers only")
}
