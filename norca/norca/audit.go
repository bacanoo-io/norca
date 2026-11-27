// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The audit records of operator write calls: one structured log record at
// INFO per call, on the server that handles it (the leader for a call
// forwarded to the leader, the named server for a call forwarded to one;
// a server that only forwards a call records nothing, so a call forwarded
// across regions or to the leader is recorded once). A call that never
// reaches the server that would handle it (no leader, say) is not recorded.
// A record carries:
//
//	audit      true
//	operation  the RPC method (Operator.RaftRemovePeerByID, ...)
//	server     the name of the server that handled the call, and region
//	identity   who called: token:<accessor ID>, alloc:<allocation ID>,
//	           client:<node ID>, or the TLS name and address; for a token
//	           also accessor_id and token_name, for a workload identity
//	           alloc_id, job_id, namespace and task; remote_ip when known
//	           (a client node's ID is only in identity: node_id is a
//	           parameter of the node calls)
//	result     success, denied (permission denied) or error, with error
//	           for the last two
//
// and the parameters of the call that auditOperations allows for its
// operation. Nothing else of the request enters the record: never a token,
// an unseal share, a key, recovery material or a secret value. Reads are not
// recorded. The records go wherever the agent's logs go (file, syslog,
// /v1/agent/monitor).
//
// An endpoint records a call with
//
//	defer x.srv.auditCall("<Service>.<Method>", args).done(&retErr)
//
// right after its forwarding, with a named error result.

// auditOperation describes the audit record of one operator write call.
type auditOperation struct {
	// params returns the parameters of the call that the record carries,
	// as key/value pairs: an allow list, never a secret.
	params func(args any) []any
}

// auditParams adapts a function of the request type to
// auditOperation.params.
func auditParams[T any](f func(*T) []any) func(any) []any {
	return func(args any) []any {
		if t, ok := args.(*T); ok && t != nil {
			return f(t)
		}
		return nil
	}
}

// noAuditParams is the auditOperation of a call recorded without
// parameters.
var noAuditParams = auditOperation{}

// auditOperations lists the operator write calls that are recorded, with the
// parameters each record carries.
var auditOperations = map[string]auditOperation{
	// Raft and autopilot.
	"Operator.RaftRemovePeerByAddress": {params: auditParams(func(a *structs.RaftPeerByAddressRequest) []any {
		return []any{"server_addr", string(a.Address)}
	})},
	"Operator.RaftRemovePeerByID": {params: auditParams(func(a *structs.RaftPeerByIDRequest) []any {
		return []any{"server_id", string(a.ID)}
	})},
	"Operator.RaftPromoteStandby": {params: auditParams(func(a *structs.RaftPromoteStandbyRequest) []any {
		return []any{"server_id", string(a.ID), "replace", string(a.Replace), "force", a.Force}
	})},
	"Operator.AutopilotSetConfiguration": {params: auditParams(func(a *structs.AutopilotSetConfigRequest) []any {
		return []any{
			"cas", a.CAS,
			"cleanup_dead_servers", a.Config.CleanupDeadServers,
			"last_contact_threshold", a.Config.LastContactThreshold,
			"max_trailing_logs", a.Config.MaxTrailingLogs,
			"min_quorum", a.Config.MinQuorum,
			"server_stabilization_time", a.Config.ServerStabilizationTime,
		}
	})},
	"Operator.SnapshotRestore": noAuditParams,

	// Scheduler.
	"Operator.SchedulerSetConfiguration": {params: auditParams(func(a *structs.SchedulerSetConfigRequest) []any {
		return []any{
			"cas", a.CAS,
			"scheduler_algorithm", string(a.Config.SchedulerAlgorithm),
			"memory_oversubscription", a.Config.MemoryOversubscriptionEnabled,
			"reject_job_registration", a.Config.RejectJobRegistration,
			"pause_eval_broker", a.Config.PauseEvalBroker,
		}
	})},
	"System.GarbageCollect":        noAuditParams,
	"System.ReconcileJobSummaries": noAuditParams,

	// Seal and keyring. The unseal share, the recovered core key and the
	// root keys never enter a record.
	"Operator.SealUnseal": {params: auditParams(func(a *structs.SealUnsealRequest) []any {
		return []any{"server_id", a.ServerID, "reset", a.Reset}
	})},
	"Operator.SealRecover": {params: auditParams(func(a *structs.SealRecoverRequest) []any {
		return []any{"reset", a.Reset, "with_share", a.Key != "", "with_core_key", len(a.CoreKey) > 0}
	})},
	"Keyring.Rotate": {params: auditParams(func(a *structs.KeyringRotateRootKeyRequest) []any {
		return []any{"algorithm", string(a.Algorithm), "full", a.Full}
	})},
	"Keyring.Update": {params: auditParams(func(a *structs.KeyringUpdateRootKeyRequest) []any {
		var keyID string
		if a.RootKey != nil && a.RootKey.Meta != nil {
			keyID = a.RootKey.Meta.KeyID
		}
		return []any{"key_id", keyID, "rekey", a.Rekey}
	})},
	"Keyring.Delete": {params: auditParams(func(a *structs.KeyringDeleteRootKeyRequest) []any {
		return []any{"key_id", a.KeyID}
	})},
	"Keyring.Restore": {params: auditParams(func(a *structs.KeyringRestoreRequest) []any {
		ids := make([]string, 0, len(a.Keys))
		for _, k := range a.Keys {
			if k != nil && k.Meta != nil {
				ids = append(ids, k.Meta.KeyID)
			}
		}
		return []any{"key_ids", ids}
	})},
	"Keyring.Rewrap": {params: auditParams(func(a *structs.KeyringRewrapRequest) []any {
		return []any{"to", a.To, "shares", a.Shares, "threshold", a.Threshold}
	})},

	// Backups. The age identity of a restore never enters a record.
	"Backup.Save": {params: auditParams(func(a *structs.BackupSaveRequest) []any {
		return []any{"pin", a.Pin, "detach", a.Detach, "labels", len(a.Labels)}
	})},
	"Backup.Update": {params: auditParams(func(a *structs.BackupUpdateRequest) []any {
		kv := []any{"backup_id", a.ID, "labels", len(a.Labels), "remove_labels", len(a.RemoveLabels)}
		if a.Pinned != nil {
			kv = append(kv, "pinned", *a.Pinned)
		}
		return kv
	})},
	"Backup.Delete": {params: auditParams(func(a *structs.BackupDeleteRequest) []any {
		return []any{"backup_ids", a.IDs}
	})},
	"Backup.Restore": {params: auditParams(func(a *structs.BackupRestoreRequest) []any {
		return []any{"backup_id", a.ID, "mode", a.Mode, "items", len(a.Items), "dry_run", a.DryRun,
			"destination", a.Destination}
	})},
	"Backup.ApproveRestore": {params: auditParams(func(a *structs.BackupRestoreApproveRequest) []any {
		return []any{"restore_id", a.RestoreID}
	})},
	"Backup.RejectRestore": {params: auditParams(func(a *structs.BackupRestoreRejectRequest) []any {
		return []any{"restore_id", a.RestoreID, "reason", a.Reason}
	})},

	// Certificate authorities. The imported private key never enters a
	// record.
	"Certificate.ImportAuthority": noAuditParams,
	"Certificate.RotateAuthority": noAuditParams,
	"Certificate.Revoke": {params: auditParams(func(a *structs.CertificateRevokeRequest) []any {
		return []any{"name", a.Name, "serial", a.Serial, "reason", a.Reason}
	})},
	"MTLS.RotateCA": noAuditParams,

	// Nodes.
	"Node.UpdateDrain": {params: auditParams(func(a *structs.NodeUpdateDrainRequest) []any {
		kv := []any{"node_id", a.NodeID, "drain", a.DrainStrategy != nil, "mark_eligible", a.MarkEligible}
		if a.DrainStrategy != nil {
			kv = append(kv, "deadline", a.DrainStrategy.Deadline, "ignore_system_jobs", a.DrainStrategy.IgnoreSystemJobs)
		}
		return kv
	})},
	"Node.UpdateEligibility": {params: auditParams(func(a *structs.NodeUpdateEligibilityRequest) []any {
		return []any{"node_id", a.NodeID, "eligibility", a.Eligibility}
	})},
	"Node.Deregister": {params: auditParams(func(a *structs.NodeDeregisterRequest) []any {
		return []any{"node_id", a.NodeID}
	})},
	"Node.BatchDeregister": {params: auditParams(func(a *structs.NodeBatchDeregisterRequest) []any {
		return []any{"node_ids", a.NodeIDs}
	})},

	// The Consul KV store on the system bus.
	"Operator.ConsulKVRestore": {params: auditParams(func(a *structs.ConsulKVRestoreRequest) []any {
		return []any{"entries", len(a.Entries)}
	})},
	"Operator.ConsulKVMigrate": {params: auditParams(func(a *structs.ConsulKVMigrateRequest) []any {
		return []any{"path", a.Path, "dry_run", a.DryRun}
	})},
}

// auditRecord is the audit record of one call, logged when the call
// returns.
type auditRecord struct {
	srv         *Server
	method      string
	args        structs.RequestWithIdentity
	isForwarded bool
}

// auditCall starts the audit record of an operator write call that this
// server handles; its done method logs it.
func (s *Server) auditCall(method string, args structs.RequestWithIdentity) *auditRecord {
	return &auditRecord{srv: s, method: method, args: args}
}

// forwarded marks a call that this server hands on to another server after
// all, which records it.
func (r *auditRecord) forwarded() {
	r.isForwarded = true
}

// done logs the record with the result of the call, *errp.
func (r *auditRecord) done(errp *error) {
	if r == nil || r.isForwarded {
		return
	}
	var err error
	if errp != nil {
		err = *errp
	}
	r.srv.logger.Named("audit").Info("operator write call", r.fields(err)...)
}

// fields returns the key/value pairs of the record.
func (r *auditRecord) fields(err error) []any {
	kv := []any{
		"audit", true,
		"operation", r.method,
		"server", r.srv.config.NodeName,
		"region", r.srv.config.Region,
	}
	kv = append(kv, auditIdentity(r.args.GetIdentity())...)
	if op := auditOperations[r.method]; op.params != nil {
		kv = append(kv, op.params(r.args)...)
	}
	switch {
	case err == nil:
		kv = append(kv, "result", "success")
	case errors.Is(err, structs.ErrPermissionDenied) || err.Error() == structs.ErrPermissionDenied.Error():
		kv = append(kv, "result", "denied", "error", err)
	default:
		kv = append(kv, "result", "error", "error", err)
	}
	return kv
}

// auditIdentity returns the key/value pairs of who made a call.
func auditIdentity(ai *structs.AuthenticatedIdentity) []any {
	kv := []any{"identity", auditIdentityName(ai)}
	if ai == nil {
		return kv
	}
	if tok := ai.ACLToken; tok != nil {
		kv = append(kv, "accessor_id", tok.AccessorID, "token_name", tok.Name)
	}
	if c := ai.Claims; c != nil {
		kv = append(kv, "alloc_id", c.AllocationID, "job_id", c.JobID, "namespace", c.Namespace, "task", c.TaskName)
	}
	if ai.RemoteIP != nil {
		kv = append(kv, "remote_ip", ai.RemoteIP.String())
	}
	return kv
}

// auditIdentityName names the caller as AuthenticatedIdentity.String does,
// except for an identity with neither a token, claims, a client ID, a TLS
// name nor a remote address (an in-process call with ACLs disabled), which
// String would render as ":<nil>": that caller is "anonymous", and one with
// only part of the TLS name and address gets the part it has.
func auditIdentityName(ai *structs.AuthenticatedIdentity) string {
	if ai == nil || ai.ACLToken != nil || ai.Claims != nil || ai.ClientID != "" {
		return ai.String()
	}
	switch {
	case ai.TLSName != "" && ai.RemoteIP != nil:
		return ai.String()
	case ai.TLSName != "":
		return "tls:" + ai.TLSName
	case ai.RemoteIP != nil:
		return "ip:" + ai.RemoteIP.String()
	default:
		return "anonymous"
	}
}
