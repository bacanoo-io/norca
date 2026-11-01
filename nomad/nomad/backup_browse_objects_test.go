// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"
	"time"

	"filippo.io/age"
	"github.com/bacanoo-io/norca/nomad/acl"
	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/helper/consulkv"
	"github.com/bacanoo-io/norca/nomad/nomad/backup"
	"github.com/bacanoo-io/norca/nomad/nomad/backup/s3fake"
	"github.com/bacanoo-io/norca/nomad/nomad/bus"
	"github.com/bacanoo-io/norca/nomad/nomad/mock"
	"github.com/bacanoo-io/norca/nomad/nomad/state"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/bacanoo-io/norca/nomad/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// backupObjectsStore holds one object of the types of backupObjects.
func backupObjectsStore(t *testing.T) *state.StateStore {
	t.Helper()
	store := state.TestStateStore(t)
	m := structs.MsgTypeTestSetup

	must.NoError(t, store.UpsertQuotaSpecs(m, 10, []*structs.QuotaSpec{{Name: "apps", Description: "quota"}}))
	must.NoError(t, store.UpsertAdmissionPolicies(m, 11, []*structs.AdmissionPolicy{{Name: "baseline"}}))
	must.NoError(t, store.UpsertNetworkPolicies(m, 12, []*structs.NetworkPolicy{{Name: "web", Namespace: "default"}}))
	must.NoError(t, store.UpsertAdminNetworkPolicies(m, 13, []*structs.AdminNetworkPolicy{{Name: "no-metadata"}}))
	must.NoError(t, store.UpsertServiceIntentions(m, 14, []*structs.ServiceIntention{
		{Namespace: "default", SourceNamespace: "default", Source: "probe", Destination: "api", Action: "allow"}}))
	kind := &structs.ObjectKind{Group: "example.com", Kind: "Widget", Namespaced: true, Description: "widgets"}
	must.NoError(t, store.UpsertObjectKind(m, 15, kind))
	must.NoError(t, store.UpsertCustomObject(m, 16, &structs.CustomObject{
		ID: "7a1c2f4e-0000-4000-8000-000000000001", Group: "example.com", Kind: "Widget",
		Namespace: "default", Name: "one"}, 0))
	must.NoError(t, store.UpsertNodePools(m, 17, []*structs.NodePool{{Name: "edge"}}))
	token := mock.ACLToken()
	must.NoError(t, store.UpsertACLTokens(m, 18, []*structs.ACLToken{token}))
	return store
}

// Every content type has a handler for browsing and for comparisons, so a
// type added to the list can't silently stay out of the full restore preview.
func TestBackupObjects_EveryContentType(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	loaded := &backup.Loaded{Record: &structs.BackupRecord{ID: "test"}, State: store}
	for _, typ := range structs.BackupContentTypes {
		f := &backupContentsFilter{namespace: "*", jobID: "job"}
		_, err := backupContents(loaded, typ, f)
		must.NoError(t, err, must.Sprint(typ))
		_, err = backupDiffEntries(backupSide{state: store}, typ, f)
		must.NoError(t, err, must.Sprint(typ))
	}
	_, err := backupContents(loaded, "unknown", &backupContentsFilter{namespace: "*"})
	must.ErrorContains(t, err, "unknown content type")
}

// The objects of the Norca features appear in the diff that the preview of a
// full restore is made from: created after the backup, the restore removes
// them.
func TestBackupDiff_NorcaObjects(t *testing.T) {
	ci.Parallel(t)

	diffs, err := backupDiff(backupSide{state: state.TestStateStore(t)}, backupSide{state: backupObjectsStore(t)},
		"", &backupContentsFilter{namespace: "*"})
	must.NoError(t, err)

	got := map[string]string{}
	for _, d := range diffs {
		must.Eq(t, structs.BackupDiffAdded, d.Change)
		got[d.Type+"/"+d.ID] = d.Namespace
	}
	for key, namespace := range map[string]string{
		"quotas/apps":                             "",
		"admission-policies/baseline":             "",
		"network-policies/web":                    "default",
		"admin-network-policies/no-metadata":      "",
		"service-intentions/default/probe -> api": "default",
		"object-kinds/example.com/Widget":         "",
		"custom-objects/example.com/Widget/one":   "default",
		"node-pools/edge":                         "",
	} {
		actual, ok := got[key]
		must.True(t, ok, must.Sprintf("%s is not in the diff: %v", key, got))
		must.Eq(t, namespace, actual, must.Sprint(key))
	}
}

// Browsing keeps to the read rules of the List endpoints of the types.
func TestBackupObjects_ACL(t *testing.T) {
	ci.Parallel(t)

	store := backupObjectsStore(t)
	aclFor := func(rules string) *acl.ACL {
		policy, err := acl.Parse(rules)
		must.NoError(t, err)
		obj, err := acl.NewACL(false, []*acl.Policy{policy})
		must.NoError(t, err)
		return obj
	}
	ids := func(typ string, aclObj *acl.ACL) []string {
		objects, ok, err := backupObjects(store, typ, &backupContentsFilter{namespace: "*", aclObj: aclObj})
		must.NoError(t, err)
		must.True(t, ok)
		out := []string{}
		for _, o := range objects {
			out = append(out, o.ID)
		}
		return out
	}

	reader := aclFor(`namespace "default" { policy = "read" }`)
	must.Eq(t, []string{"default/probe -> api"}, ids(structs.BackupContentServiceIntentions, reader))
	must.SliceEmpty(t, ids(structs.BackupContentQuotas, reader))
	must.SliceEmpty(t, ids(structs.BackupContentAdmissionPolicies, reader))
	must.SliceEmpty(t, ids(structs.BackupContentAdminNetworkPolicies, reader))
	must.SliceEmpty(t, ids(structs.BackupContentACLTokens, reader))
	must.SliceEmpty(t, ids(structs.BackupContentCustomObjects, reader))

	other := aclFor(`namespace "other" { policy = "read" }`)
	must.SliceEmpty(t, ids(structs.BackupContentServiceIntentions, other))

	must.Eq(t, []string{"apps"}, ids(structs.BackupContentQuotas, aclFor(`quota { policy = "read" }`)))
	must.Len(t, 1, ids(structs.BackupContentACLTokens, acl.ManagementACL))
}

// The token of an ACL stub is its accessor ID, never the secret.
func TestBackupObjects_NoSecrets(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	token := mock.ACLToken()
	must.NoError(t, store.UpsertACLTokens(structs.MsgTypeTestSetup, 10, []*structs.ACLToken{token}))
	objects, ok, err := backupObjects(store, structs.BackupContentACLTokens, &backupContentsFilter{namespace: "*"})
	must.NoError(t, err)
	must.True(t, ok)
	must.Len(t, 1, objects)
	must.Eq(t, token.AccessorID, objects[0].ID)
	must.StrNotContains(t, objects[0].ID+objects[0].Detail, token.SecretID)
}

func TestSplitIntentionItemID(t *testing.T) {
	ci.Parallel(t)

	ns, source, destination, err := splitIntentionItemID("default/probe -> api")
	must.NoError(t, err)
	must.Eq(t, "default", ns)
	must.Eq(t, "probe", source)
	must.Eq(t, "api", destination)

	for _, id := range []string{"probe -> api", "default/probe", "/probe -> api", "default/probe -> "} {
		_, _, _, err := splitIntentionItemID(id)
		must.ErrorContains(t, err, "invalid service intention", must.Sprint(id))
	}
}

func TestOrderBackupRestoreItems(t *testing.T) {
	ci.Parallel(t)

	items := []*structs.BackupRestoreItem{
		{Type: structs.BackupRestoreItemNamespace, ID: "a"},
		{Type: structs.BackupRestoreItemJob, ID: "b"},
		{Type: structs.BackupRestoreItemQuota, ID: "c"},
	}
	orderBackupRestoreItems(items)
	must.Eq(t, "c", items[0].ID)
	must.Eq(t, "a", items[1].ID)
	must.Eq(t, "b", items[2].ID)
}

// Live leases that are not in the backup are counted; revoked and expired
// ones have nothing left to revoke.
func TestSecretLeasesLost(t *testing.T) {
	ci.Parallel(t)

	now := time.Now()
	last := now.Add(2 * time.Hour)
	lease := func(id string, revoked bool, maxExpire time.Time) *structs.SecretLease {
		return &structs.SecretLease{ID: id, Namespace: "default", Engine: "nats", Role: "app",
			Revoked: revoked, MaxExpireTime: maxExpire.UnixNano()}
	}

	backupState := state.TestStateStore(t)
	current := state.TestStateStore(t)
	for _, store := range []*state.StateStore{backupState, current} {
		must.NoError(t, store.ApplySecretEngine(structs.MsgTypeTestSetup, 5, &structs.SecretEngineApplyRequest{
			Engine: &structs.SecretEngine{Namespace: "default", Name: "nats", Type: "nats"}}))
	}
	must.NoError(t, backupState.ApplySecretLeases(structs.MsgTypeTestSetup, 10,
		&structs.SecretLeaseApplyRequest{Leases: []*structs.SecretLease{lease("kept", false, last)}}))
	must.NoError(t, current.ApplySecretLeases(structs.MsgTypeTestSetup, 10, &structs.SecretLeaseApplyRequest{Leases: []*structs.SecretLease{
		lease("kept", false, last),
		lease("new-1", false, now.Add(time.Hour)),
		lease("new-2", false, last),
		lease("revoked", true, last),
		lease("expired", false, now.Add(-time.Hour)),
	}}))

	count, expires, err := secretLeasesLost(current, backupState, now)
	must.NoError(t, err)
	must.Eq(t, 2, count)
	must.Eq(t, last.UnixNano(), expires.UnixNano())
}

func TestBusKV_RestoreEndpoint(t *testing.T) {
	ci.Parallel(t)

	srv, root, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)
	js := busJS(t, srv)
	waitBus(t, func() (bool, error) {
		_, err := js.KeyValue(t.Context(), bus.ConsulKVBucket)
		return err == nil, err
	})

	entry := func(key, value string) *structs.ConsulKVRestoreEntry {
		return &structs.ConsulKVRestoreEntry{Namespace: "default", Path: consulkv.DefaultPath, Key: key, Value: []byte(value)}
	}
	restore := func(token string, entries ...*structs.ConsulKVRestoreEntry) (int, error) {
		var resp structs.ConsulKVRestoreResponse
		err := msgpackrpc.CallWithCodec(codec, "Operator.ConsulKVRestore", &structs.ConsulKVRestoreRequest{
			Entries:      entries,
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token},
		}, &resp)
		return resp.Restored, err
	}

	// It overwrites every namespace: operator write is not enough.
	writer := mock.CreatePolicyAndToken(t, srv.State(), 1001, "operator-write", `operator { policy = "write" }`)
	_, err := restore(writer.SecretID, entry("a", "1"))
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	_, err = restore(root.SecretID, entry("", "1"))
	must.ErrorContains(t, err, "needs a key")

	n, err := restore(root.SecretID, entry("a", "1"), entry("b", "2"))
	must.NoError(t, err)
	must.Eq(t, 2, n)

	// A restore replaces the bucket.
	n, err = restore(root.SecretID, entry("b", "3"))
	must.NoError(t, err)
	must.Eq(t, 1, n)
	export, err := srv.exportConsulKV(t.Context())
	must.NoError(t, err)
	must.Len(t, 1, export.Entries)
	must.Eq(t, "b", export.Entries[0].Key)
	must.Eq(t, []byte("3"), export.Entries[0].Value)
}

// Every restorable type goes the whole way: written through its endpoint,
// backed up, deleted and restored through the endpoint again, so the objects
// of a backup pass the validation of their writes.
func TestBackupEndpoint_RestoreObjects(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	srv, cleanup := TestServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.NumSchedulers = 0
		c.Backup = testBackupConfig(s3, identity, false)
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)

	write := structs.WriteRequest{Region: "global"}
	call := func(method string, req, resp any) {
		t.Helper()
		must.NoError(t, msgpackrpc.CallWithCodec(codec, method, req, resp))
	}
	var generic structs.GenericResponse

	quota := mockQuotaSpec()
	quota.Name = "apps"
	call("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{Quotas: []*structs.QuotaSpec{quota}, WriteRequest: write}, &generic)
	call("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces: []*structs.Namespace{{Name: "prod", Quota: "apps"}}, WriteRequest: write}, &generic)
	call("AdmissionPolicy.UpsertAdmissionPolicies", &structs.AdmissionPolicyUpsertRequest{
		AdmissionPolicies: []*structs.AdmissionPolicy{testAdmissionPolicy("baseline", nil, "", `job.Priority < 90`)},
		WriteRequest:      write}, &generic)
	call("NetworkPolicy.UpsertNetworkPolicies", &structs.NetworkPolicyUpsertRequest{
		NetworkPolicies: []*structs.NetworkPolicy{testNetworkPolicy("prod", "web", nil)}, WriteRequest: write}, &generic)
	call("NetworkPolicy.UpsertAdminNetworkPolicies", &structs.AdminNetworkPolicyUpsertRequest{
		AdminNetworkPolicies: []*structs.AdminNetworkPolicy{{Name: "no-metadata", Priority: 10,
			Egress: []*structs.NetworkPolicyRule{{Action: "deny",
				Peers: []*structs.NetworkPolicyPeer{{CIDR: []string{"169.254.169.254/32"}}}}}}},
		WriteRequest: write}, &generic)
	intention := &structs.ServiceIntention{Namespace: "prod", SourceNamespace: "prod", Source: "probe",
		Destination: "api", Action: "allow"}
	intentionWrite := structs.WriteRequest{Region: "global", Namespace: "prod"}
	call("ServiceIntention.Upsert", &structs.ServiceIntentionUpsertRequest{
		Intentions: []*structs.ServiceIntention{intention}, WriteRequest: intentionWrite}, &generic)
	var kindResp structs.ObjectKindUpsertResponse
	call("ObjectKind.UpsertObjectKind", &structs.ObjectKindUpsertRequest{
		ObjectKind: testPGKind(true), WriteRequest: write}, &kindResp)
	var objectResp structs.CustomObjectWriteResponse
	call("CustomObject.Apply", &structs.CustomObjectApplyRequest{
		Object:       testPGObject("prod", "one", map[string]any{"version": "16"}),
		WriteRequest: intentionWrite}, &objectResp)

	must.NoError(t, srv.backup.SetConfig(testBackupConfig(s3, identity, true)))
	record := waitForBackup(t, srv, 1)

	store := srv.fsm.State()
	index, _ := store.LatestIndex()
	m := structs.MsgTypeTestSetup
	must.NoError(t, store.DeleteCustomObject(m, index+1, "db.example.com", "PostgresCluster", "prod", "one", 0))
	must.NoError(t, store.DeleteServiceIntentions(m, index+2, []*structs.ServiceIntention{intention}))
	must.NoError(t, store.DeleteAdminNetworkPolicies(m, index+3, []string{"no-metadata"}))
	must.NoError(t, store.DeleteNetworkPolicies(m, index+4, "prod", []string{"web"}))
	must.NoError(t, store.DeleteAdmissionPolicies(m, index+5, []string{"baseline"}))
	must.NoError(t, store.DeleteNamespaces(index+6, []string{"prod"}))
	must.NoError(t, store.DeleteQuotaSpecs(m, index+7, []string{"apps"}))

	restore := func(dryRun bool, items ...*structs.BackupRestoreItem) []*structs.BackupRestoreItemResult {
		t.Helper()
		var resp structs.BackupRestoreResponse
		call("Backup.Restore", &structs.BackupRestoreRequest{ID: record.ID, AgeIdentity: identity.String(),
			Items: items, DryRun: dryRun, WriteRequest: write}, &resp)
		return resp.Results
	}
	namespaceItem := &structs.BackupRestoreItem{Type: structs.BackupRestoreItemNamespace, ID: "prod"}

	// The quota of the namespace is gone: refused, also in a dry run.
	results := restore(true, namespaceItem)
	must.StrContains(t, results[0].Error, `uses quota "apps", which no longer exists`)
	results = restore(false, namespaceItem)
	must.StrContains(t, results[0].Error, `uses quota "apps", which no longer exists`)

	// The objects of the package manager are refused.
	results = restore(false, &structs.BackupRestoreItem{Type: structs.BackupRestoreItemCustomObject,
		Namespace: "prod", ID: structs.ReleaseObjectGroup + "/Release/app"})
	must.StrContains(t, results[0].Error, "one by one")

	// Given after the namespace, the quota is still restored first.
	items := []*structs.BackupRestoreItem{
		namespaceItem,
		{Type: structs.BackupRestoreItemQuota, ID: "apps"},
		{Type: structs.BackupRestoreItemAdmissionPolicy, ID: "baseline"},
		{Type: structs.BackupRestoreItemNetworkPolicy, Namespace: "prod", ID: "web"},
		{Type: structs.BackupRestoreItemAdminNetworkPolicy, ID: "no-metadata"},
		{Type: structs.BackupRestoreItemServiceIntention, Namespace: "prod", ID: "prod/probe -> api"},
		{Type: structs.BackupRestoreItemCustomObject, Namespace: "prod", ID: "db.example.com/PostgresCluster/one"},
	}
	for _, r := range restore(true, items...) {
		must.Eq(t, "", r.Error, must.Sprint(r.Item.Type))
		must.Eq(t, "create", r.Action, must.Sprint(r.Item.Type))
	}
	must.Nil(t, mustQuota(t, store, "apps"))
	for _, r := range restore(false, items...) {
		must.Eq(t, "", r.Error, must.Sprint(r.Item.Type))
	}

	must.NotNil(t, mustQuota(t, store, "apps"))
	ns, err := store.NamespaceByName(nil, "prod")
	must.NoError(t, err)
	must.Eq(t, "apps", ns.Quota)
	policy, err := store.NetworkPolicyByName(nil, "prod", "web")
	must.NoError(t, err)
	must.NotNil(t, policy)
	admission, err := store.AdmissionPolicyByName(nil, "baseline")
	must.NoError(t, err)
	must.NotNil(t, admission)
	admin, err := store.AdminNetworkPolicyByName(nil, "no-metadata")
	must.NoError(t, err)
	must.NotNil(t, admin)
	si, err := store.ServiceIntentionByKey(nil, "prod", "api", "prod", "probe")
	must.NoError(t, err)
	must.NotNil(t, si)
	obj, err := store.CustomObjectByName(nil, "db.example.com", "PostgresCluster", "prod", "one")
	must.NoError(t, err)
	must.NotNil(t, obj)
	must.Eq(t, "16", obj.Spec["version"].(string))
}

func mustQuota(t *testing.T, store *state.StateStore, name string) *structs.QuotaSpec {
	t.Helper()
	quota, err := store.QuotaSpecByName(nil, name)
	must.NoError(t, err)
	return quota
}
