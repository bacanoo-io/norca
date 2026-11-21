// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"
	"slices"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The operator of certificates acts with the rights of the servers, so what
// a writer of its objects may cause is checked when the object is written:
// the names against what the namespace may ask for, and the right of the
// writer on the secret the certificate is written to. The operator checks
// the names again before every issuance, because the namespace may change.

// errCertificateOrderWrite is the error of writing an order through the
// object API.
func errCertificateOrderWrite() error {
	return structs.NewErrRPCCodedf(http.StatusForbidden,
		"objects of the kind %s/%s are written by the servers only", structs.OperatorsGroup, structs.CertificateOrderKind)
}

// certificateIssuer returns the issuer of a name and its spec, or nil.
func certificateIssuer(snap *state.StateSnapshot, name string) (*structs.CustomObject, *structs.CertificateIssuerSpec, error) {
	obj, err := snap.CustomObjectByName(nil, structs.OperatorsGroup, structs.CertificateIssuerKind, "", name)
	if err != nil || obj == nil {
		return nil, nil, err
	}
	spec := &structs.CertificateIssuerSpec{}
	if err := structs.DecodeObjectPart(obj.Spec, spec); err != nil {
		return nil, nil, err
	}
	return obj, spec, nil
}

// checkCertificateApply checks an object of a kind of certificates that is
// written through the object API. obj has the defaults of its schema.
func checkCertificateApply(snap *state.StateSnapshot, aclObj *acl.ACL, obj *structs.CustomObject) error {
	switch obj.Kind {
	case structs.CertificateOrderKind:
		return errCertificateOrderWrite()
	case structs.CertificateKind:
	default:
		return nil
	}

	spec := &structs.CertificateSpec{}
	if err := structs.DecodeObjectPart(obj.Spec, spec); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid certificate: %v", err)
	}
	if err := spec.Validate(); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid certificate: %v", err)
	}

	// The certificate ends in the secret: its writer must be allowed to
	// write that secret.
	if !aclObj.AllowSecretOperation(obj.Namespace, spec.SecretPath, acl.NamespaceCapabilityWriteSecret) {
		return structs.NewErrRPCCodedf(http.StatusForbidden,
			"a certificate needs the right to write its secret %q in the namespace %q", spec.SecretPath, obj.Namespace)
	}

	ns, err := snap.NamespaceByName(nil, obj.Namespace)
	if err != nil {
		return err
	}
	// An issuer that does not exist yet is the operator's to report; only
	// the built-in one signs IP addresses, so those need it now.
	_, issuer, err := certificateIssuer(snap, spec.Issuer)
	if err != nil {
		return err
	}
	if issuer == nil && len(spec.IPAddresses) > 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"the issuer %q does not exist: a certificate with IP addresses needs a built-in issuer", spec.Issuer)
	}
	builtin := issuer != nil && issuer.Builtin != nil
	if err := structs.CheckCertificateNames(spec, ns, builtin || issuer == nil); err != nil {
		return structs.NewErrRPCCodedf(http.StatusForbidden, "%v", err)
	}

	// The servers revoke the certificates of the object, and delete its
	// secret if it says so, before it goes (review finding ATM-24f). The
	// finalizer is on from the first write, as on a tenant: a deletion that
	// comes before the next pass of the operator finds it. The operator
	// takes it off when the spec no longer asks for either.
	if !obj.Deleting() && spec.NeedsOperatorAtDeletion() && !slices.Contains(obj.Finalizers, structs.OperatorsFinalizer) {
		obj.Finalizers = append(obj.Finalizers, structs.OperatorsFinalizer)
	}
	return nil
}

// checkCertificateStatusWrite refuses status writes through the object API
// where the status is the servers': certificates and built-in issuers. The
// status of an external issuer and of an order is written by the controller
// of the issuer.
func checkCertificateStatusWrite(existing *structs.CustomObject) error {
	switch existing.Kind {
	case structs.CertificateKind:
		return structs.NewErrRPCCodedf(http.StatusForbidden, "the status of a certificate is written by the servers only")
	case structs.CertificateIssuerKind:
		spec := &structs.CertificateIssuerSpec{}
		if err := structs.DecodeObjectPart(existing.Spec, spec); err != nil || spec.Builtin != nil {
			return structs.NewErrRPCCodedf(http.StatusForbidden, "the status of a built-in issuer is written by the servers only")
		}
	}
	return nil
}

// checkCertificateDelete refuses the deletion of an order to all but
// management tokens: the servers delete orders when they are done.
func checkCertificateDelete(aclObj *acl.ACL, existing *structs.CustomObject) error {
	if existing.Kind == structs.CertificateOrderKind && aclObj != nil && !aclObj.IsManagement() {
		return errCertificateOrderWrite()
	}
	return nil
}
