// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// newTenantTermTest returns a tenant test server whose tenant operator ends
// the leadership term of the server, once armed, right before it writes or
// deletes the resource target: a leader change in the middle of a pass. The
// term ends as revokeLeadership ends it; the server stays the Raft leader,
// as a deposed leader that has not noticed it yet seems to itself.
func newTenantTermTest(t *testing.T, target tenantResourceRef) (*certTest, *atomic.Bool, *atomic.Bool) {
	t.Helper()
	armed, fired := &atomic.Bool{}, &atomic.Bool{}
	var srv atomic.Pointer[Server]
	hook := func(ref tenantResourceRef) {
		if ref.key() == target.key() && armed.CompareAndSwap(true, false) {
			srv.Load().releases.setLeader(false)
			fired.Store(true)
		}
	}
	s, root, cleanup := TestACLServer(t, func(c *Config) {
		c.tenantBeforeWrite = hook
		c.tenantBeforeDelete = hook
	})
	t.Cleanup(cleanup)
	srv.Store(s)
	testutil.WaitForLeader(t, s.RPC)
	waitTenantKind(t, s)
	return &certTest{t: t, s: s, token: root.SecretID}, armed, fired
}

// tenantTermSpec is a tenant with a namespace and a quota: the quota is
// written first, the namespace second, and deleted the other way round.
var tenantTermSpec = map[string]any{
	"namespace": map[string]any{"description": "term"},
	"quota": map[string]any{"region": map[string]any{
		"global": map[string]any{"cpu": 1000, "memory": 1024},
	}},
}

// A pass whose leadership term ends in the middle writes nothing more: not
// the resource it was about to write, not the next one, not the status. The
// pass of the next term decides (review finding CON-6c). The pass went on
// writing through the endpoints, which forward to the new leader.
func TestTenantOperator_LeadershipEndStopsPass(t *testing.T) {
	ci.Parallel(t)
	quotaRef := tenantResourceRef{Type: structs.TenantResourceQuota, Name: "team-term"}
	c, armed, fired := newTenantTermTest(t, quotaRef)
	store := c.s.State()
	armed.Store(true)

	_, err := c.apply("", structs.TenantKind, "team-term", tenantTermSpec)
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(fired.Load),
		wait.Timeout(30*time.Second), wait.Gap(20*time.Millisecond)))

	// Long enough for a pass that goes on to write the quota, the namespace
	// and the status.
	time.Sleep(2 * time.Second)
	q, err := store.QuotaSpecByName(nil, "team-term")
	must.NoError(t, err)
	must.Nil(t, q, must.Sprint("the pass wrote the quota after its term ended"))
	ns, err := store.NamespaceByName(nil, "team-term")
	must.NoError(t, err)
	must.Nil(t, ns, must.Sprint("the pass wrote the namespace after its term ended"))
	obj := c.get("", structs.TenantKind, "team-term")
	must.NotNil(t, obj)
	must.SliceEmpty(t, operatorConditions(obj.Status),
		must.Sprintf("the pass wrote the status after its term ended: %v", obj.Status))

	// The next term's pass creates what the tenant wants.
	c.s.releases.setLeader(true)
	waitTenant(c, "team-term", "ready in the next term", tenantReady)
	q, _ = store.QuotaSpecByName(nil, "team-term")
	must.NotNil(t, q)
	ns, _ = store.NamespaceByName(nil, "team-term")
	must.NotNil(t, ns)
}

// The deletion of a tenant whose leadership term ends in the middle deletes
// nothing more and keeps the finalizer: the pass of the next term finishes
// it (CON-6c). The deletion went on through the endpoints.
func TestTenantOperator_LeadershipEndStopsDeletion(t *testing.T) {
	ci.Parallel(t)
	nsRef := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "team-term"}
	c, armed, fired := newTenantTermTest(t, nsRef)
	store := c.s.State()

	_, err := c.apply("", structs.TenantKind, "team-term", tenantTermSpec)
	must.NoError(t, err)
	waitTenant(c, "team-term", "ready", tenantReady)

	armed.Store(true)
	must.NoError(t, c.delete("", structs.TenantKind, "team-term"))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(fired.Load),
		wait.Timeout(30*time.Second), wait.Gap(20*time.Millisecond)))

	time.Sleep(2 * time.Second)
	ns, err := store.NamespaceByName(nil, "team-term")
	must.NoError(t, err)
	must.NotNil(t, ns, must.Sprint("the deletion deleted the namespace after its term ended"))
	q, err := store.QuotaSpecByName(nil, "team-term")
	must.NoError(t, err)
	must.NotNil(t, q, must.Sprint("the deletion deleted the quota after its term ended"))
	obj := c.get("", structs.TenantKind, "team-term")
	must.NotNil(t, obj, must.Sprint("the deletion let the tenant go after its term ended"))
	must.SliceContains(t, obj.Finalizers, structs.OperatorsFinalizer)

	c.s.releases.setLeader(true)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("", structs.TenantKind, "team-term") == nil
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))
	ns, _ = store.NamespaceByName(nil, "team-term")
	must.Nil(t, ns)
	q, _ = store.QuotaSpecByName(nil, "team-term")
	must.Nil(t, q)
}
