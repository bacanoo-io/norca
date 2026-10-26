// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"errors"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Auditor records the audit record of a fetch the client made from an
// external provider (docs/design/secrets-plan.md, "Audit"). Fetches from
// the built-in store are not recorded by the client: the servers record
// every value read of the store themselves, with the same workload identity
// as the accessor, so a client record would only duplicate it.
type Auditor interface {
	Record(id *Identity, provider, path, version string, err error) error
}

// RPCAuditor sends the records to the servers (Secret.RecordAccess), which
// publish them on the SecretAccess topic and write them to the audit file
// like their own records.
type RPCAuditor struct {
	rpc    config.RPCer
	region string
}

// NewRPCAuditor returns an auditor sending records over server RPC.
func NewRPCAuditor(rpc config.RPCer, region string) *RPCAuditor {
	return &RPCAuditor{rpc: rpc, region: region}
}

// Record sends the record of a fetch with its result; err is the fetch
// error. The record never contains a value.
func (a *RPCAuditor) Record(id *Identity, provider, path, version string, err error) error {
	if a == nil || a.rpc == nil || id == nil {
		return nil
	}
	rec := &structs.SecretAccessEvent{
		Path:            path,
		Operation:       structs.SecretOpRead,
		Result:          AuditResult(err),
		Provider:        provider,
		ProviderVersion: version,
	}
	if err != nil {
		rec.Error = err.Error()
	}
	args := &structs.SecretRecordAccessRequest{
		Record: rec,
		WriteRequest: structs.WriteRequest{
			Region:    a.region,
			Namespace: id.Namespace,
			AuthToken: id.Token,
		},
	}
	var reply structs.GenericResponse
	return a.rpc.RPC(structs.SecretRecordAccessRPCMethod, args, &reply)
}

// FetchAudited fetches a secret and, for providers that don't audit their
// value reads themselves, records the client audit record of the fetch. A
// failure to record is returned as auditErr; it doesn't fail the fetch.
func FetchAudited(ctx context.Context, p Provider, req *Request, auditor Auditor) (sec *Secret, err, auditErr error) {
	if req.Version != 0 && !p.Capabilities().Versions {
		return nil, NewError(p.Name(), KindInvalid, errors.New("the provider does not support pinned versions")), nil
	}
	sec, err = p.Fetch(ctx, req)
	if !p.Capabilities().AuditsReads && auditor != nil && ctx.Err() == nil {
		version := ""
		if sec != nil {
			version = sec.Version
		}
		auditErr = auditor.Record(req.Identity, p.Name(), req.Path, version, err)
	}
	return sec, err, auditErr
}

// AuditResult is the audit result of a fetch error.
func AuditResult(err error) string {
	if err == nil {
		return structs.SecretAccessResultOK
	}
	switch KindOf(err) {
	case KindDenied:
		return structs.SecretAccessResultDenied
	case KindNotFound:
		return structs.SecretAccessResultNotFound
	case KindUnavailable:
		return structs.SecretAccessResultSealed
	case KindInvalid:
		return structs.SecretAccessResultInvalid
	}
	return structs.SecretAccessResultError
}
