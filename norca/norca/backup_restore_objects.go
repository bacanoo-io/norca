// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// orderBackupRestoreItems restores the quotas first: the namespaces of the
// same request may refer to them.
func orderBackupRestoreItems(items []*structs.BackupRestoreItem) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Type == structs.BackupRestoreItemQuota && items[j].Type != structs.BackupRestoreItemQuota
	})
}

// checkNamespaceQuota refuses a namespace whose quota would not exist after
// the restore: a namespace restored on its own must not point at nothing.
func (r *backupRestorer) checkNamespaceQuota(ns *structs.Namespace) error {
	if ns.Quota == "" {
		return nil
	}
	current, err := r.b.srv.State().QuotaSpecByName(nil, ns.Quota)
	if err != nil {
		return err
	}
	if current != nil {
		return nil
	}
	restored, err := r.loaded.State.QuotaSpecByName(nil, ns.Quota)
	if err != nil {
		return err
	}
	if restored == nil {
		return fmt.Errorf("namespace %q uses quota %q, which exists neither now nor in the backup", ns.Name, ns.Quota)
	}
	if !slices.ContainsFunc(r.args.Items, func(i *structs.BackupRestoreItem) bool {
		return i.Type == structs.BackupRestoreItemQuota && i.ID == ns.Quota
	}) {
		return fmt.Errorf("namespace %q uses quota %q, which no longer exists: restore the quota too", ns.Name, ns.Quota)
	}
	return nil
}

func (r *backupRestorer) restoreQuota(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	quota, err := r.loaded.State.QuotaSpecByName(nil, item.ID)
	if err != nil {
		return err
	}
	if quota == nil {
		return fmt.Errorf("quota %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().QuotaSpecByName(nil, item.ID)
	if err != nil {
		return err
	}
	r.describe(result, "quota", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	restored := *quota
	req := &structs.QuotaSpecUpsertRequest{
		Quotas:       []*structs.QuotaSpec{&restored},
		WriteRequest: r.writeRequest(""),
	}
	var resp structs.GenericResponse
	if err := r.b.srv.RPC("Quota.UpsertQuotaSpecs", req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

func (r *backupRestorer) restoreAdmissionPolicy(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	policy, err := r.loaded.State.AdmissionPolicyByName(nil, item.ID)
	if err != nil {
		return err
	}
	if policy == nil {
		return fmt.Errorf("admission policy %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().AdmissionPolicyByName(nil, item.ID)
	if err != nil {
		return err
	}
	r.describe(result, "admission policy", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	restored := *policy
	req := &structs.AdmissionPolicyUpsertRequest{
		AdmissionPolicies: []*structs.AdmissionPolicy{&restored},
		WriteRequest:      r.writeRequest(""),
	}
	var resp structs.GenericResponse
	if err := r.b.srv.RPC("AdmissionPolicy.UpsertAdmissionPolicies", req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

func (r *backupRestorer) restoreNetworkPolicy(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	policy, err := r.loaded.State.NetworkPolicyByName(nil, item.Namespace, item.ID)
	if err != nil {
		return err
	}
	if policy == nil {
		return fmt.Errorf("network policy %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().NetworkPolicyByName(nil, item.Namespace, item.ID)
	if err != nil {
		return err
	}
	r.describe(result, "network policy", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	restored := *policy
	req := &structs.NetworkPolicyUpsertRequest{
		NetworkPolicies: []*structs.NetworkPolicy{&restored},
		WriteRequest:    r.writeRequest(item.Namespace),
	}
	var resp structs.GenericResponse
	if err := r.b.srv.RPC("NetworkPolicy.UpsertNetworkPolicies", req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

func (r *backupRestorer) restoreAdminNetworkPolicy(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	policy, err := r.loaded.State.AdminNetworkPolicyByName(nil, item.ID)
	if err != nil {
		return err
	}
	if policy == nil {
		return fmt.Errorf("admin network policy %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().AdminNetworkPolicyByName(nil, item.ID)
	if err != nil {
		return err
	}
	r.describe(result, "admin network policy", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	restored := *policy
	req := &structs.AdminNetworkPolicyUpsertRequest{
		AdminNetworkPolicies: []*structs.AdminNetworkPolicy{&restored},
		WriteRequest:         r.writeRequest(""),
	}
	var resp structs.GenericResponse
	if err := r.b.srv.RPC("NetworkPolicy.UpsertAdminNetworkPolicies", req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

// splitIntentionItemID parses the ID of a service intention in browsing and
// diffs: "<source namespace>/<source> -> <destination>".
func splitIntentionItemID(id string) (sourceNamespace, source, destination string, err error) {
	from, destination, ok := strings.Cut(id, " -> ")
	if ok {
		sourceNamespace, source, ok = strings.Cut(from, "/")
	}
	if !ok || sourceNamespace == "" || source == "" || destination == "" {
		return "", "", "", fmt.Errorf("invalid service intention %q, must be \"<source namespace>/<source> -> <destination>\"", id)
	}
	return sourceNamespace, source, destination, nil
}

func (r *backupRestorer) restoreServiceIntention(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	sourceNamespace, source, destination, err := splitIntentionItemID(item.ID)
	if err != nil {
		return err
	}
	intention, err := r.loaded.State.ServiceIntentionByKey(nil, item.Namespace, destination, sourceNamespace, source)
	if err != nil {
		return err
	}
	if intention == nil {
		return fmt.Errorf("service intention %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().ServiceIntentionByKey(nil, item.Namespace, destination, sourceNamespace, source)
	if err != nil {
		return err
	}
	r.describe(result, "service intention", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	restored := *intention
	req := &structs.ServiceIntentionUpsertRequest{
		Intentions:   []*structs.ServiceIntention{&restored},
		WriteRequest: r.writeRequest(item.Namespace),
	}
	var resp structs.GenericResponse
	if err := r.b.srv.RPC("ServiceIntention.Upsert", req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

// restoreExternalService registers an external service of the backup again,
// through the endpoint that registers them, so that its permissions apply: the
// register-external-service capability, and operator write for a service
// without checks. A service registered through the Consul-compatible API is
// not restored: its application registers it again. A service owned by a
// client node goes back to that node, which must exist and needs node write;
// a service with a checked_by selector gets its checking node from the
// servers. The states of the checks start again from their initial status.
func (r *backupRestorer) restoreExternalService(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	reg, err := r.loaded.State.GetServiceRegistrationByID(nil, item.Namespace, item.ID)
	if err != nil {
		return err
	}
	if reg == nil || !reg.IsExternal() {
		return fmt.Errorf("external service %q not found in the backup", item.ID)
	}
	if reg.External.Source == structs.ExternalServiceSourceConsul {
		return fmt.Errorf("external service %q was registered through the Consul-compatible API: its application registers it again", item.ID)
	}

	current, err := r.b.srv.State().GetServiceRegistrationByID(nil, item.Namespace, item.ID)
	if err != nil {
		return err
	}
	if current != nil && !current.IsExternal() {
		return fmt.Errorf("service ID %q is used by a registration of allocation %s", item.ID, current.AllocID)
	}
	r.describe(result, "external service", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	restored := reg.Copy()
	restored.CreateIndex, restored.ModifyIndex = 0, 0
	req := &structs.ExternalServiceUpsertRequest{
		Service:      restored,
		WriteRequest: r.writeRequest(item.Namespace),
	}
	if reg.External.CheckedBy == nil {
		req.NodeID = reg.NodeID
	}
	var resp structs.ExternalServiceUpsertResponse
	if err := r.b.srv.RPC(structs.ServiceRegistrationUpsertExternalRPCMethod, req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	return nil
}

// restoreCustomObject applies the spec, labels and owners of an object of the
// backup; its status is left to its controller. The objects of the package
// manager are not restored one by one: a release owns jobs and other
// resources, and a full restore is the way back.
func (r *backupRestorer) restoreCustomObject(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	parts := strings.SplitN(item.ID, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("invalid custom object %q, must be \"<group>/<kind>/<name>\"", item.ID)
	}
	group, kindName, name := parts[0], parts[1], parts[2]
	if group == structs.ReleaseObjectGroup {
		return fmt.Errorf("objects of %s can't be restored one by one: they own other resources", group)
	}

	kind, err := r.b.srv.State().ObjectKindByName(nil, group, kindName)
	if err != nil {
		return err
	}
	if kind == nil {
		return fmt.Errorf("the type definition %s/%s does not exist: install it first", group, kindName)
	}
	namespace := objectNamespace(kind, item.Namespace)

	obj, err := r.loaded.State.CustomObjectByName(nil, group, kindName, namespace, name)
	if err != nil {
		return err
	}
	if obj == nil {
		return fmt.Errorf("custom object %q not found in the backup", item.ID)
	}
	current, err := r.b.srv.State().CustomObjectByName(nil, group, kindName, namespace, name)
	if err != nil {
		return err
	}
	r.describe(result, "custom object", item.ID, current != nil, func() uint64 { return current.ModifyIndex })
	if r.args.DryRun {
		return nil
	}

	restored := *obj
	req := &structs.CustomObjectApplyRequest{Object: &restored, WriteRequest: r.writeRequest(namespace)}
	var resp structs.CustomObjectWriteResponse
	if err := r.b.srv.RPC("CustomObject.Apply", req, &resp); err != nil {
		return err
	}
	result.Index = resp.Index
	if resp.Object != nil {
		result.Index = resp.Object.ModifyIndex
	}
	return nil
}
