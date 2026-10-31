// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/nats-io/nats.go/jetstream"
)

// maxBackupRestoreItems bounds the items of a single restore request.
const maxBackupRestoreItems = 100

// Restore restores objects from a backup. The selective mode re-creates
// each item with the normal write RPC on behalf of the caller, so the
// caller's namespace permissions, the admission chain and the quotas apply,
// and every restored job becomes a new, traceable version. The full mode
// previews a full region restore, or creates a pending request that a
// second administrator approves with ApproveRestore.
func (b *Backup) Restore(args *structs.BackupRestoreRequest, reply *structs.BackupRestoreResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.Restore", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "restore"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowBackupRead() {
		return structs.ErrPermissionDenied
	}
	if err := b.checkVersion(); err != nil {
		return err
	}

	switch args.Mode {
	case "", structs.BackupRestoreModeSelective:
	case structs.BackupRestoreModeFull:
		return b.requestFullRestore(args, reply)
	default:
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid restore mode %q", args.Mode)
	}
	if len(args.Items) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one item to restore")
	}
	if len(args.Items) > maxBackupRestoreItems {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "at most %d items can be restored at once", maxBackupRestoreItems)
	}
	for _, item := range args.Items {
		if err := validateBackupRestoreItem(item); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
		}
	}

	ctx, cancel := b.backupContext()
	defer cancel()
	loaded, err := b.load(ctx, args.ID, args.Destination, args.AgeIdentity)
	if err != nil {
		return err
	}

	var keyring *Encrypter
	if loaded.Keys != nil {
		if keyring, err = newBackupKeyring(loaded.Keys); err != nil {
			return err
		}
	}

	var record *structs.BackupRestore
	if !args.DryRun {
		record = &structs.BackupRestore{
			ID:          uuid.Generate(),
			BackupID:    loaded.Record.ID,
			Mode:        structs.BackupRestoreModeSelective,
			Items:       args.Items,
			Status:      structs.BackupRestoreStatusRunning,
			RequestedBy: args.GetIdentity().String(),
			CreateTime:  time.Now().UTC(),
			ModifyTime:  time.Now().UTC(),
		}
		if _, err := b.writeRestoreRecord(record); err != nil {
			return err
		}
	}

	r := &backupRestorer{b: b, args: args, loaded: loaded, keyring: keyring}
	var failed int
	for _, item := range args.Items {
		result := r.restore(item)
		if result.Error != "" {
			failed++
		}
		reply.Results = append(reply.Results, result)
	}

	if record != nil {
		record = record.Copy()
		record.Results = reply.Results
		record.ModifyTime = time.Now().UTC()
		record.Status = structs.BackupRestoreStatusComplete
		if failed > 0 {
			record.Status = structs.BackupRestoreStatusFailed
			record.StatusDescription = fmt.Sprintf("%d of %d items failed", failed, len(args.Items))
		}
		index, err := b.writeRestoreRecord(record)
		if err != nil {
			return err
		}
		reply.Restore = record
		reply.Index = index
	}
	return nil
}

func (b *Backup) writeRestoreRecord(record *structs.BackupRestore) (uint64, error) {
	_, index, err := b.srv.raftApply(structs.BackupRestoreUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupRestoreUpsertRequest{Restores: []*structs.BackupRestore{record}})
	return index, err
}

func validateBackupRestoreItem(item *structs.BackupRestoreItem) error {
	if item == nil {
		return fmt.Errorf("empty restore item")
	}
	if item.ID == "" {
		return fmt.Errorf("restore item of type %q has no ID", item.Type)
	}
	switch item.Type {
	case structs.BackupRestoreItemJob, structs.BackupRestoreItemVariable, structs.BackupRestoreItemSecret:
		if item.Namespace == "" {
			item.Namespace = structs.DefaultNamespace
		}
	case structs.BackupRestoreItemNamespace, structs.BackupRestoreItemACLPolicy, structs.BackupRestoreItemACLRole:
	case structs.BackupRestoreItemConsulKV:
		item.Namespace = consulKVNamespace(item.Namespace)
	default:
		return fmt.Errorf("invalid restore item type %q", item.Type)
	}
	if item.Version != nil && item.Type != structs.BackupRestoreItemJob && item.Type != structs.BackupRestoreItemSecret {
		return fmt.Errorf("a version can only be restored for jobs and secrets")
	}
	if item.WithMetadata && item.Type != structs.BackupRestoreItemSecret {
		return fmt.Errorf("the metadata option is only used for secrets")
	}
	if item.CheckIndex != nil && item.Type != structs.BackupRestoreItemVariable && item.Type != structs.BackupRestoreItemSecret &&
		item.Type != structs.BackupRestoreItemConsulKV {
		return fmt.Errorf("a check index can only be given for Variables, secrets and Consul KV keys")
	}
	return nil
}

// backupRestorer restores the items of a request.
type backupRestorer struct {
	b       *Backup
	args    *structs.BackupRestoreRequest
	loaded  *backup.Loaded
	keyring *Encrypter
}

// writeRequest returns the write request of an inner RPC, made with the
// caller's token so the caller's permissions apply.
func (r *backupRestorer) writeRequest(namespace string) structs.WriteRequest {
	return structs.WriteRequest{
		Region:    r.b.srv.Region(),
		Namespace: namespace,
		AuthToken: r.args.AuthToken,
	}
}

func (r *backupRestorer) restore(item *structs.BackupRestoreItem) *structs.BackupRestoreItemResult {
	result := &structs.BackupRestoreItemResult{Item: item}
	var err error
	switch item.Type {
	case structs.BackupRestoreItemJob:
		err = r.restoreJob(item, result)
	case structs.BackupRestoreItemVariable:
		err = r.restoreVariable(item, result)
	case structs.BackupRestoreItemSecret:
		err = r.restoreSecret(item, result)
	case structs.BackupRestoreItemNamespace:
		err = r.restoreNamespace(item, result)
	case structs.BackupRestoreItemACLPolicy:
		err = r.restoreACLPolicy(item, result)
	case structs.BackupRestoreItemACLRole:
		err = r.restoreACLRole(item, result)
	case structs.BackupRestoreItemConsulKV:
		err = r.restoreConsulKV(item, result)
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func (r *backupRestorer) restoreJob(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	store := r.loaded.State
	var job *structs.Job
	var err error
	if item.Version != nil {
		job, err = store.JobByIDAndVersion(nil, item.Namespace, item.ID, *item.Version)
	} else {
		job, err = store.JobByID(nil, item.Namespace, item.ID)
	}
	if err != nil {
		return err
	}
	if job == nil {
		if item.Version != nil {
			return fmt.Errorf("job %q version %d not found in the backup", item.ID, *item.Version)
		}
		return fmt.Errorf("job %q not found in the backup", item.ID)
	}

	current, err := r.b.srv.State().JobByID(nil, item.Namespace, item.ID)
	if err != nil {
		return err
	}
	if current == nil {
		result.Action = "create"
		result.Description = fmt.Sprintf("re-create job %q from version %d of the backup", item.ID, job.Version)
	} else {
		result.Action = "update"
		result.CurrentIndex = current.JobModifyIndex
		result.Description = fmt.Sprintf("register version %d of the backup as a new version of job %q (current version %d)",
			job.Version, item.ID, current.Version)
	}
	if r.args.DryRun {
		return nil
	}

	req := &structs.JobRegisterRequest{
		Job:          job.Copy(),
		WriteRequest: r.writeRequest(item.Namespace),
	}
	var resp structs.JobRegisterResponse
	if err := r.b.srv.RPC("Job.Register", req, &resp); err != nil {
		return err
	}
	result.Index = resp.JobModifyIndex
	if resp.Warnings != "" {
		result.Description += "; warnings: " + strings.TrimSpace(resp.Warnings)
	}
	return nil
}

func (r *backupRestorer) restoreVariable(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	encrypted, err := r.loaded.State.GetVariable(nil, item.Namespace, item.ID)
	if err != nil {
		return err
	}
	if encrypted == nil {
		return fmt.Errorf("variable %q not found in the backup", item.ID)
	}

	// Decrypt with the backup's own keyring; fall back to the current one,
	// which still holds the keys that were not removed since.
	var items structs.VariableItems
	if r.keyring != nil {
		items, err = decryptBackupVariable(r.keyring, encrypted)
	}
	if r.keyring == nil || err != nil {
		items, err = decryptBackupVariable(r.b.srv.encrypter, encrypted)
	}
	if err != nil {
		return fmt.Errorf("failed to decrypt variable %q: %v", item.ID, err)
	}

	current, err := r.b.srv.State().GetVariable(nil, item.Namespace, item.ID)
	if err != nil {
		return err
	}
	checkIndex := uint64(0)
	if item.CheckIndex != nil {
		checkIndex = *item.CheckIndex
	}
	if current == nil {
		result.Action = "create"
		result.Description = fmt.Sprintf("re-create variable %q (%d keys)", item.ID, len(items))
	} else {
		result.Action = "update"
		result.CurrentIndex = current.ModifyIndex
		result.Description = fmt.Sprintf("overwrite variable %q (%d keys) at modify index %d", item.ID, len(items), current.ModifyIndex)
		if item.CheckIndex == nil {
			msg := fmt.Sprintf("variable %q exists (modify index %d); give that check index to overwrite it",
				item.ID, current.ModifyIndex)
			if r.args.DryRun {
				result.Description += "; " + msg
				return nil
			}
			return fmt.Errorf("%s", msg)
		}
	}
	if r.args.DryRun {
		return nil
	}

	req := &structs.VariablesApplyRequest{
		Op: structs.VarOpCAS,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{
				Namespace:   item.Namespace,
				Path:        item.ID,
				ModifyIndex: checkIndex,
			},
			Items: items,
		},
		WriteRequest: r.writeRequest(item.Namespace),
	}
	var resp structs.VariablesApplyResponse
	if err := r.b.srv.RPC(structs.VariablesApplyRPCMethod, req, &resp); err != nil {
		return err
	}
	if !resp.IsOk() {
		if resp.Conflict != nil {
			return fmt.Errorf("variable %q was modified in the meantime (modify index %d)", item.ID, resp.Conflict.ModifyIndex)
		}
		if resp.Error != nil {
			return resp.Error
		}
		return fmt.Errorf("variable %q was not restored: %s", item.ID, resp.Result)
	}
	if resp.Output != nil {
		result.Index = resp.Output.ModifyIndex
	}
	return nil
}

func (r *backupRestorer) restoreNamespace(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	ns, err := r.loaded.State.NamespaceByName(nil, item.ID)
	if err != nil {
		return err
	}
	if ns == nil {
		return fmt.Errorf("namespace %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().NamespaceByName(nil, item.ID)
	if err != nil {
		return err
	}
	r.describe(result, "namespace", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	req := &structs.NamespaceUpsertRequest{
		Namespaces:   []*structs.Namespace{ns.Copy()},
		WriteRequest: r.writeRequest(""),
	}
	var resp structs.GenericResponse
	if err := r.b.srv.RPC("Namespace.UpsertNamespaces", req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

func (r *backupRestorer) restoreACLPolicy(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	policy, err := r.loaded.State.ACLPolicyByName(nil, item.ID)
	if err != nil {
		return err
	}
	if policy == nil {
		return fmt.Errorf("ACL policy %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().ACLPolicyByName(nil, item.ID)
	if err != nil {
		return err
	}
	r.describe(result, "ACL policy", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	restored := *policy
	req := &structs.ACLPolicyUpsertRequest{
		Policies:     []*structs.ACLPolicy{&restored},
		WriteRequest: r.writeRequest(""),
	}
	var resp structs.GenericResponse
	if err := r.b.srv.RPC(structs.ACLUpsertPoliciesRPCMethod, req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

func (r *backupRestorer) restoreACLRole(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	role, err := r.loaded.State.GetACLRoleByName(nil, item.ID)
	if err != nil {
		return err
	}
	if role == nil {
		return fmt.Errorf("ACL role %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().GetACLRoleByName(nil, item.ID)
	if err != nil {
		return err
	}
	r.describe(result, "ACL role", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	// An existing role is updated in place; a deleted one is created anew.
	restored := role.Copy()
	restored.ID = ""
	if current != nil {
		restored.ID = current.ID
	}
	req := &structs.ACLRolesUpsertRequest{
		ACLRoles:     []*structs.ACLRole{restored},
		WriteRequest: r.writeRequest(""),
	}
	var resp structs.ACLRolesUpsertResponse
	if err := r.b.srv.RPC(structs.ACLUpsertRolesRPCMethod, req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

func (r *backupRestorer) describe(result *structs.BackupRestoreItemResult, kind, name string, exists bool, index func() uint64) {
	if exists {
		result.Action = "update"
		result.CurrentIndex = index()
		result.Description = fmt.Sprintf("overwrite %s %q with the backup", kind, name)
		return
	}
	result.Action = "create"
	result.Description = fmt.Sprintf("re-create %s %q from the backup", kind, name)
}

// restoreConsulKV writes a key of the Consul KV of the backup into the bucket
// of the system bus. Like a Variables entry, an existing key is only
// overwritten with its current revision as the check index. The caller needs
// the write capability of the Variables path of the key.
func (r *backupRestorer) restoreConsulKV(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	path, id := splitConsulKVItemID(item.ID)
	entry := r.loaded.ConsulKV.Entry(item.Namespace, path, id)
	if entry == nil {
		return fmt.Errorf("Consul KV key %q not found in the backup", item.ID)
	}
	aclObj, err := r.b.srv.ResolveACL(r.args)
	if err != nil {
		return err
	}
	if aclObj != nil && !aclObj.AllowVariableOperation(item.Namespace,
		consulkv.ACLPath(path, id), acl.VariablesCapabilityWrite, nil) {
		return structs.ErrPermissionDenied
	}

	ctx, cancel := context.WithTimeout(r.b.srv.shutdownCtx, 30*time.Second)
	defer cancel()
	kv, done, err := r.b.srv.consulKVBucket(ctx)
	if err != nil {
		return err
	}
	defer done()
	if kv == nil {
		return fmt.Errorf("the system bus has no Consul KV bucket")
	}
	key := consulKVNATSKey(entry)
	current, err := kv.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		current = nil
	case err != nil:
		return err
	}
	if current == nil {
		result.Action = "create"
		result.Description = fmt.Sprintf("re-create Consul KV key %q (%d bytes)", item.ID, len(entry.Value))
	} else {
		result.Action = "update"
		result.CurrentIndex = current.Revision()
		result.Description = fmt.Sprintf("overwrite Consul KV key %q (%d bytes) at revision %d", item.ID, len(entry.Value), current.Revision())
		if item.CheckIndex == nil || *item.CheckIndex == 0 {
			msg := fmt.Sprintf("Consul KV key %q exists (revision %d); give that check index to overwrite it", item.ID, current.Revision())
			if r.args.DryRun {
				result.Description += "; " + msg
				return nil
			}
			return fmt.Errorf("%s", msg)
		}
	}
	if r.args.DryRun {
		return nil
	}
	var revision uint64
	if current == nil {
		revision, err = kv.Create(ctx, key, entry.Value)
	} else {
		revision, err = kv.Update(ctx, key, entry.Value, *item.CheckIndex)
	}
	if err != nil {
		return fmt.Errorf("Consul KV key %q was not restored: %w", item.ID, err)
	}
	result.Index = revision
	return nil
}
