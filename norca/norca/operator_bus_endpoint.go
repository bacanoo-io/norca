// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"

	"github.com/bacanoo-io/norca/norca/helper/consulkv"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// BusStatus returns the status of the system bus of the server that
// receives the request, or of the server named by ServerID.
func (op *Operator) BusStatus(args *structs.BusStatusRequest, reply *structs.BusStatusResponse) error {
	// The status is local to a server: never forwarded to the leader.
	args.AllowStale = true

	authErr := op.srv.Authenticate(op.ctx, args)
	if done, err := op.srv.forward("Operator.BusStatus", args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if aclObj, err := op.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowOperatorRead() {
		return structs.ErrPermissionDenied
	}

	if args.ServerID != "" {
		target, err := op.srv.getServer(op.srv.config.Region, args.ServerID)
		if err != nil {
			return err
		}
		if target.Name != op.srv.LocalMember().Name {
			return op.srv.forwardServer(target, "Operator.BusStatus", args, reply)
		}
	}

	status, err := op.srv.busStatus()
	if err != nil {
		return err
	}
	reply.Status = status
	return nil
}

// ConsulKVRestore replaces the Consul KV bucket of the system bus with the
// keys of a backup. Like the restore of the keyring it needs a management
// token: it overwrites keys of every namespace.
func (op *Operator) ConsulKVRestore(args *structs.ConsulKVRestoreRequest, reply *structs.ConsulKVRestoreResponse) error {
	authErr := op.srv.Authenticate(op.ctx, args)
	if done, err := op.srv.forward("Operator.ConsulKVRestore", args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	aclObj, err := op.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if aclObj != nil && !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}

	entries := make([]*consulkv.Entry, 0, len(args.Entries))
	for _, e := range args.Entries {
		if e == nil || e.Key == "" {
			return structs.NewErrRPCCodedf(400, "every entry of the Consul KV needs a key")
		}
		entries = append(entries, &consulkv.Entry{Namespace: e.Namespace, Path: e.Path, Key: e.Key, Value: e.Value})
	}

	ctx, cancel := context.WithTimeout(op.srv.shutdownCtx, consulKVMigrateTimeout)
	defer cancel()
	if err := op.srv.importConsulKV(ctx, entries, true); err != nil {
		return err
	}
	reply.Restored = len(entries)
	reply.Index = op.srv.raft.AppliedIndex()
	return nil
}

// ConsulKVMigrate copies the keys of the Consul KV kept in Variables into
// the bucket of the system bus. It needs operator write; only the keys the
// token may read as Variables are copied.
func (op *Operator) ConsulKVMigrate(args *structs.ConsulKVMigrateRequest, reply *structs.ConsulKVMigrateResponse) error {
	authErr := op.srv.Authenticate(op.ctx, args)
	if done, err := op.srv.forward("Operator.ConsulKVMigrate", args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	aclObj, err := op.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if aclObj != nil && !aclObj.AllowOperatorWrite() {
		return structs.ErrPermissionDenied
	}

	ctx, cancel := context.WithTimeout(op.srv.shutdownCtx, consulKVMigrateTimeout)
	defer cancel()
	result, err := op.srv.migrateConsulKV(ctx, aclObj, args.RequestNamespace(), args.Path, args.DryRun)
	if err != nil {
		return err
	}
	reply.Result = result
	reply.Index = op.srv.raft.AppliedIndex()
	return nil
}
