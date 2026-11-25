// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// The tests of this file state what it takes to mark a job as the dependent
// of an object (the meta norca.owner; review finding JOG-20).

const testOwnerMarkFleetKind = "Fleet"

// ownerMeta is the owner mark of a job, as the controller SDK writes it.
func ownerMeta(group, kind, namespace, name string) map[string]string {
	return map[string]string{
		structs.JobMetaOwner:     structs.ObjectOwnerMetaValue(group, kind, namespace, name),
		structs.JobMetaOwnerKind: structs.ObjectKindRef(group, kind),
	}
}

// registerJob registers a job with the meta given, in its namespace.
func (c *certTest) registerJob(job *structs.Job) error {
	var resp structs.JobRegisterResponse
	return c.s.RPC("Job.Register", &structs.JobRegisterRequest{Job: job.Copy(), WriteRequest: c.write(job.Namespace)}, &resp)
}

func markedJob(namespace, id string, meta map[string]string) *structs.Job {
	job := mock.Job()
	job.ID, job.Name, job.Namespace = id, id, namespace
	job.Meta = meta
	return job
}

// jobOwnerMarkServer is ownerMarkServer with the namespace "other" and the
// cluster-scoped kind example.com/Fleet.
func jobOwnerMarkServer(t *testing.T) (*certTest, func()) {
	t.Helper()
	c, cleanup := ownerMarkServer(t)
	other := mock.Namespace()
	other.Name = "other"
	must.NoError(t, c.s.State().UpsertNamespaces(990, []*structs.Namespace{other}))
	var resp structs.ObjectKindUpsertResponse
	must.NoError(t, c.s.RPC("ObjectKind.UpsertObjectKind", &structs.ObjectKindUpsertRequest{
		ObjectKind:   &structs.ObjectKind{Group: testOwnerGroup, Kind: testOwnerMarkFleetKind, Namespaced: false, Schema: `{"type": "object"}`},
		WriteRequest: c.write(""),
	}, &resp))
	return c, cleanup
}

// A job carries the owner mark only if its submitter has "write" or
// "manage-finalizers" on the owner: in the namespace of the job, in another
// namespace, or cluster-scoped. The refusal is that of an object that names
// an owner, and is the same whether the owner exists or not.
func TestJobOwnerMark_NeedsRightOnOwner(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	// Jobs of both namespaces, no object of any kind.
	submitter := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "submitter", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}
namespace "other" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`))
	// Jobs of both namespaces, the finalizers of the Databases of "default"
	// and of the Fleets.
	controller := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "controller", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
  object "example.com/Database" { capabilities = ["manage-finalizers"] }
}
namespace "other" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}
namespace "*" {
  object "example.com/Fleet" { capabilities = ["manage-finalizers"] }
}`))
	// Jobs of "default", and the Databases of "other" written.
	writer := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1005, "writer", `namespace "default" {
  capabilities = ["submit-job"]
}
namespace "other" {
  object "example.com/Database" { capabilities = ["write"] }
}`))

	must.NoError(t, c.applyOwned(testOwnerGroup, testOwnerKind, "shop", map[string]any{}))

	sameNamespace := ownerMeta(testOwnerGroup, testOwnerKind, "default", "shop")
	missing := ownerMeta(testOwnerGroup, testOwnerKind, "default", "no-such-database")
	clusterScoped := ownerMeta(testOwnerGroup, testOwnerMarkFleetKind, "", "main")

	// Without the right the mark is refused, and no job is written.
	err := submitter.registerJob(markedJob("default", "same", sameNamespace))
	must.Eq(t, 403, errCode(t, err), must.Sprint("the owner mark was accepted without a right on the owner"))
	must.ErrorContains(t, err, `owner example.com/Database "shop" in namespace "default"`)
	must.ErrorContains(t, err, "requires the write or manage-finalizers capability on the owner")
	missingErr := submitter.registerJob(markedJob("default", "same", missing))
	must.Eq(t, 403, errCode(t, missingErr))
	must.Eq(t, strings.ReplaceAll(err.Error(), `"shop"`, `"no-such-database"`), missingErr.Error())

	err = submitter.registerJob(markedJob("other", "across", sameNamespace))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, `owner example.com/Database "shop" in namespace "default"`)

	err = submitter.registerJob(markedJob("default", "fleet", clusterScoped))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, `owner example.com/Fleet "main" cluster-scoped`)

	// The rights of the namespace of the job are not those of the owner.
	err = writer.registerJob(markedJob("default", "same", sameNamespace))
	must.Eq(t, 403, errCode(t, err))

	for _, id := range []string{"same", "fleet"} {
		job, err := c.s.State().JobByID(nil, "default", id)
		must.NoError(t, err)
		must.Nil(t, job)
	}
	job, err := c.s.State().JobByID(nil, "other", "across")
	must.NoError(t, err)
	must.Nil(t, job)

	// With the right on the owner the mark is written: an owner of the
	// namespace of the job, of another namespace, a cluster-scoped one, and
	// one that does not exist.
	must.NoError(t, controller.registerJob(markedJob("default", "same", sameNamespace)))
	must.NoError(t, controller.registerJob(markedJob("other", "across", sameNamespace)))
	must.NoError(t, controller.registerJob(markedJob("default", "fleet", clusterScoped)))
	must.NoError(t, controller.registerJob(markedJob("default", "early", missing)))
	must.NoError(t, writer.registerJob(markedJob("default", "written",
		ownerMeta(testOwnerGroup, testOwnerKind, "other", "shop"))))
	must.True(t, structs.JobOwnedBy(c.jobIn("other", "across").Meta, testOwnerGroup, testOwnerKind, "default", "shop"))
	must.True(t, structs.JobOwnedBy(c.jobIn("default", "fleet").Meta, testOwnerGroup, testOwnerMarkFleetKind, "", "main"))

	// A job without the mark takes no right on any object.
	must.NoError(t, submitter.registerJob(markedJob("default", "plain", map[string]string{"team": "shop"})))

	// A management token is not restricted.
	must.NoError(t, c.registerJob(markedJob("default", "root", sameNamespace)))
}

// An update that keeps the mark of the stored job is not checked again;
// changing the mark is a new mark, and removing it takes no right on the
// owner.
func TestJobOwnerMark_UpdateOfAMarkedJob(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	submitter := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "submitter", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`))
	controller := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "controller", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
  object "example.com/Database" { capabilities = ["manage-finalizers"] }
}`))

	shop := ownerMeta(testOwnerGroup, testOwnerKind, "default", "shop")
	must.NoError(t, controller.registerJob(markedJob("default", "app", shop)))

	// The submitter updates the job with its mark.
	update := markedJob("default", "app", shop)
	update.Priority = 70
	must.NoError(t, submitter.registerJob(update))
	stored := c.jobIn("default", "app")
	must.Eq(t, 70, stored.Priority)
	must.Eq(t, uint64(1), stored.Version)
	must.True(t, structs.JobOwnedBy(stored.Meta, testOwnerGroup, testOwnerKind, "default", "shop"))

	// Another owner, or another kind of the owner, is a new mark.
	err := submitter.registerJob(markedJob("default", "app", ownerMeta(testOwnerGroup, testOwnerKind, "default", "site")))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, `owner example.com/Database "site" in namespace "default"`)
	withoutKind := map[string]string{structs.JobMetaOwner: shop[structs.JobMetaOwner]}
	err = submitter.registerJob(markedJob("default", "app", withoutKind))
	must.Eq(t, 403, errCode(t, err))
	must.True(t, structs.JobOwnedBy(c.jobIn("default", "app").Meta, testOwnerGroup, testOwnerKind, "default", "shop"))

	// The reverted version has the mark of the stored job: a revert by the
	// submitter passes.
	var revert structs.JobRegisterResponse
	must.NoError(t, c.s.RPC("Job.Revert", &structs.JobRevertRequest{
		JobID: "app", JobVersion: 0, WriteRequest: submitter.write("default"),
	}, &revert))
	must.Eq(t, 50, c.jobIn("default", "app").Priority)

	// Removing the mark takes no right on the owner; putting it back does,
	// by a registration and by a revert to a version that has it.
	must.NoError(t, submitter.registerJob(markedJob("default", "app", map[string]string{"team": "shop"})))
	must.MapNotContainsKey(t, c.jobIn("default", "app").Meta, structs.JobMetaOwner)
	err = submitter.registerJob(markedJob("default", "app", shop))
	must.Eq(t, 403, errCode(t, err))
	err = c.s.RPC("Job.Revert", &structs.JobRevertRequest{
		JobID: "app", JobVersion: 0, WriteRequest: submitter.write("default"),
	}, &revert)
	must.Eq(t, 403, errCode(t, err))
	must.MapNotContainsKey(t, c.jobIn("default", "app").Meta, structs.JobMetaOwner)
	must.NoError(t, controller.registerJob(markedJob("default", "app", shop)))
}

// A stopped job is no holder of its mark: the garbage collector does nothing
// to it and no deletion of the owner asks for it, so registering it again
// with the mark makes it a dependent anew, and takes the right on the owner
// that a new mark takes (review finding JOG-24). Before the fix the mark of a
// job that the collector, or anybody, had stopped counted as kept, and the
// right to submit jobs was enough to run the job again as the dependent.
func TestJobOwnerMark_StoppedJobHoldsNoMark(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	submitter := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "submitter", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`))
	controller := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "controller", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
  object "example.com/Database" { capabilities = ["manage-finalizers"] }
}`))
	stop := func(id string) {
		t.Helper()
		var resp structs.JobDeregisterResponse
		must.NoError(t, c.s.RPC("Job.Deregister", &structs.JobDeregisterRequest{JobID: id, WriteRequest: c.write("default")}, &resp))
		must.True(t, c.jobIn("default", id).Stop)
	}

	shop := ownerMeta(testOwnerGroup, testOwnerKind, "default", "shop")
	must.NoError(t, controller.registerJob(markedJob("default", "app", shop)))
	stop("app")
	must.True(t, structs.JobOwnedBy(c.jobIn("default", "app").Meta, testOwnerGroup, testOwnerKind, "default", "shop"))

	// With the mark the stopped job is registered again only with the right
	// on the owner; a revert to a version with the mark is a registration.
	err := submitter.registerJob(markedJob("default", "app", shop))
	must.Error(t, err, must.Sprint("a stopped job was registered again with its owner mark without a right on the owner"))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, `owner example.com/Database "shop" in namespace "default"`)
	var revert structs.JobRegisterResponse
	err = c.s.RPC("Job.Revert", &structs.JobRevertRequest{
		JobID: "app", JobVersion: 0, WriteRequest: submitter.write("default"),
	}, &revert)
	must.Eq(t, 403, errCode(t, err))
	must.True(t, c.jobIn("default", "app").Stop)

	// Without the mark it takes no right on the owner.
	must.NoError(t, submitter.registerJob(markedJob("default", "app", map[string]string{"team": "shop"})))
	must.False(t, c.jobIn("default", "app").Stop)
	must.MapNotContainsKey(t, c.jobIn("default", "app").Meta, structs.JobMetaOwner)

	// With the right on the owner the stopped job runs again as the
	// dependent, and then its mark is kept by whoever may update it.
	must.NoError(t, controller.registerJob(markedJob("default", "app", shop)))
	stop("app")
	must.NoError(t, controller.registerJob(markedJob("default", "app", shop)))
	running := c.jobIn("default", "app")
	must.False(t, running.Stop)
	must.True(t, structs.JobOwnedBy(running.Meta, testOwnerGroup, testOwnerKind, "default", "shop"))
	update := markedJob("default", "app", shop)
	update.Priority = 70
	must.NoError(t, submitter.registerJob(update))
	must.Eq(t, 70, c.jobIn("default", "app").Priority)
}

// windowConfigEntries runs a function when a registration writes the
// configuration entry of its ingress gateway to Consul: after the endpoint
// read the stored job and checked the registration against it, and before
// the registration is applied.
type windowConfigEntries struct {
	ConsulConfigsAPI
	inWindow atomic.Pointer[func()]
}

func (w *windowConfigEntries) SetIngressCE(context.Context, string, string, *structs.ConsulIngressConfigEntry) error {
	if f := w.inWindow.Swap(nil); f != nil {
		(*f)()
	}
	return nil
}

func (w *windowConfigEntries) SetTerminatingCE(context.Context, string, string, *structs.ConsulTerminatingConfigEntry) error {
	return nil
}

func (w *windowConfigEntries) Stop() {}

// A registration that keeps the mark of the stored job without the right on
// the owner holds for the job the endpoint read: if the job is stopped, loses
// its mark or is purged before the registration is applied, the registration
// is refused, and writes nothing (review finding JOG-24, the window between
// the snapshot of the endpoint and the apply). Before the fix it was applied
// to whatever was stored then.
func TestJobOwnerMark_KeptMarkHoldsForTheJobRead(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()
	entries := &windowConfigEntries{}
	c.s.consulConfigEntries = entries

	submitter := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "submitter", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`))
	controller := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "controller", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
  object "example.com/Database" { capabilities = ["manage-finalizers"] }
}`))

	shop := ownerMeta(testOwnerGroup, testOwnerKind, "default", "shop")
	gateway := func(id string, meta map[string]string, priority int) *structs.Job {
		job := mock.ConnectIngressGatewayJob("host", false)
		job.ID, job.Name, job.Namespace = id, id, "default"
		job.Meta = meta
		job.Priority = priority
		return job
	}
	deregister := func(id string, purge bool) {
		var resp structs.JobDeregisterResponse
		must.NoError(t, c.s.RPC("Job.Deregister", &structs.JobDeregisterRequest{JobID: id, Purge: purge, WriteRequest: c.write("default")}, &resp))
	}
	stored := func(id string) *structs.Job {
		t.Helper()
		job, err := c.s.State().JobByID(nil, "default", id)
		must.NoError(t, err)
		return job
	}

	tests := []struct {
		name string
		// change is what happens to the stored job in the window.
		change func(id string)
		// after checks the stored job: the change, and nothing of the
		// registration.
		after func(t *testing.T, job *structs.Job)
	}{
		{
			name:   "stopped",
			change: func(id string) { deregister(id, false) },
			after: func(t *testing.T, job *structs.Job) {
				must.NotNil(t, job)
				must.True(t, job.Stop, must.Sprint("the stopped job runs again as the dependent"))
				must.Eq(t, 50, job.Priority)
			},
		},
		{
			name: "mark removed",
			change: func(id string) {
				must.NoError(t, controller.registerJob(gateway(id, map[string]string{"team": "shop"}, 50)))
			},
			after: func(t *testing.T, job *structs.Job) {
				must.NotNil(t, job)
				must.MapNotContainsKey(t, job.Meta, structs.JobMetaOwner, must.Sprint("the mark is back"))
				must.Eq(t, 50, job.Priority)
			},
		},
		{
			name:   "purged",
			change: func(id string) { deregister(id, true) },
			after: func(t *testing.T, job *structs.Job) {
				must.Nil(t, job, must.Sprint("a new job was written with the mark"))
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := fmt.Sprintf("gw-%d", i)
			must.NoError(t, controller.registerJob(gateway(id, shop, 50)))

			change := func() { tt.change(id) }
			entries.inWindow.Store(&change)
			err := submitter.registerJob(gateway(id, shop, 70))
			must.Nil(t, entries.inWindow.Load(), must.Sprint("the registration did not reach the window"))
			must.Error(t, err, must.Sprint("the registration was applied to a job that is not the one it was checked against"))
			must.Eq(t, 409, errCode(t, err))
			must.ErrorContains(t, err, structs.JobMetaOwner)
			tt.after(t, stored(id))
		})
	}

	// Nothing changes in the window: the registration is applied.
	must.NoError(t, controller.registerJob(gateway("gw-kept", shop, 50)))
	noChange := func() {}
	entries.inWindow.Store(&noChange)
	must.NoError(t, submitter.registerJob(gateway("gw-kept", shop, 70)))
	must.Eq(t, 70, stored("gw-kept").Priority)

	// A caller with the right on the owner needs no stored mark: its
	// registration is applied to the job as it is then, as any registration.
	must.NoError(t, controller.registerJob(gateway("gw-own", shop, 50)))
	stop := func() { deregister("gw-own", false) }
	entries.inWindow.Store(&stop)
	must.NoError(t, controller.registerJob(gateway("gw-own", shop, 70)))
	own := stored("gw-own")
	must.False(t, own.Stop)
	must.Eq(t, 70, own.Priority)
}

// A value that names no owner is refused: no right can be checked for it.
func TestJobOwnerMark_InvalidValue(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	controller := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "controller", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
  object "example.com/*" { capabilities = ["write", "manage-finalizers"] }
}`))

	for _, value := range []string{
		"",
		"shop",
		"default/crd/example-com/Database",
		"default/crd/example-com/Database/shop/more",
		"default/objects/example-com/Database/shop",
		"default/crd/example---com/Database/shop",
		"default/crd//Database/shop",
		"not a namespace/crd/example-com/Database/shop",
		"*/crd/example-com/Database/shop",
	} {
		err := controller.registerJob(markedJob("default", "app", map[string]string{structs.JobMetaOwner: value}))
		must.Eq(t, 400, errCode(t, err), must.Sprintf("value %q", value))
		must.ErrorContains(t, err, structs.JobMetaOwner)
	}

	// The kind of the mark is the kind its value names.
	meta := ownerMeta(testOwnerGroup, testOwnerKind, "default", "shop")
	meta[structs.JobMetaOwnerKind] = structs.ObjectKindRef(testOwnerGroup, testOwnerMarkHookKind)
	err := controller.registerJob(markedJob("default", "app", meta))
	must.Eq(t, 400, errCode(t, err))
	must.ErrorContains(t, err, structs.JobMetaOwnerKind)

	job, err := c.s.State().JobByID(nil, "default", "app")
	must.NoError(t, err)
	must.Nil(t, job)

	// The value alone is a mark, of the kind it names.
	must.NoError(t, controller.registerJob(markedJob("default", "app",
		map[string]string{structs.JobMetaOwner: structs.ObjectOwnerMetaValue(testOwnerGroup, testOwnerKind, "default", "shop")})))

	// A management token is not restricted.
	must.NoError(t, c.registerJob(markedJob("default", "root", map[string]string{structs.JobMetaOwner: "shop"})))
}

// Without ACLs nothing is checked.
func TestJobOwnerMark_ACLsDisabled(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	codec := rpcClient(t, s)

	for id, meta := range map[string]map[string]string{
		"marked":  ownerMeta(testOwnerGroup, testOwnerKind, "default", "shop"),
		"invalid": {structs.JobMetaOwner: "shop"},
	} {
		job := markedJob("default", id, meta)
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
			Job: job, WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}, &resp))
	}
}

// A dispatch sets no reserved meta key (the owner mark, its kind, the
// managing identity), whether the parameterized job lists the key or not,
// and a parameterized job that lists one is not registered. The child of a
// marked parent has the mark of the parent.
func TestJobOwnerMark_Dispatch(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	controller := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "controller", `namespace "default" {
  capabilities = ["submit-job", "dispatch-job", "read-job", "list-jobs"]
  object "example.com/Database" { capabilities = ["manage-finalizers"] }
}`))
	dispatcher := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "dispatcher", `namespace "default" {
  capabilities = ["dispatch-job", "read-job", "list-jobs"]
}`))

	parameterized := func(id string, meta map[string]string, optional ...string) *structs.Job {
		job := mock.BatchJob()
		job.ID, job.Name, job.Namespace = id, id, "default"
		job.Meta = meta
		job.ParameterizedJob = &structs.ParameterizedJobConfig{
			Payload:      structs.DispatchPayloadOptional,
			MetaOptional: optional,
		}
		return job
	}
	dispatch := func(as *certTest, id string, meta map[string]string) (*structs.JobDispatchResponse, error) {
		var resp structs.JobDispatchResponse
		err := c.s.RPC("Job.Dispatch", &structs.JobDispatchRequest{JobID: id, Meta: meta, WriteRequest: as.write("default")}, &resp)
		return &resp, err
	}

	// A parameterized job that lists a reserved key is refused, for every
	// caller: the dispatch could not set it.
	for _, key := range []string{structs.JobMetaOwner, structs.JobMetaOwnerKind, structs.JobMetaManagedBy} {
		for _, as := range []*certTest{controller, c} {
			err := as.registerJob(parameterized("lists", nil, "team", key))
			must.ErrorContains(t, err, "is reserved and can't be listed in meta_optional", must.Sprintf("key %q", key))
			job := parameterized("lists", nil, "team")
			job.ParameterizedJob.MetaRequired = []string{key}
			err = as.registerJob(job)
			must.ErrorContains(t, err, "is reserved and can't be listed in meta_required", must.Sprintf("key %q", key))
		}
	}
	job, err := c.s.State().JobByID(nil, "default", "lists")
	must.NoError(t, err)
	must.Nil(t, job)

	// A parameterized job that lists the keys, as one stored before the
	// check: the dispatch does not set them.
	legacy := parameterized("legacy", nil, "team", structs.JobMetaOwner, structs.JobMetaOwnerKind, structs.JobMetaManagedBy)
	must.NoError(t, c.s.State().UpsertJob(structs.MsgTypeTestSetup, 2000, nil, legacy))
	mark := ownerMeta(testOwnerGroup, testOwnerKind, "default", "shop")
	for _, as := range []*certTest{dispatcher, controller, c} {
		_, err := dispatch(as, "legacy", mark)
		must.Eq(t, 400, errCode(t, err), must.Sprint("a dispatch set the owner mark"))
		must.ErrorContains(t, err, "is reserved: a dispatch can't set it")
		_, err = dispatch(as, "legacy", map[string]string{structs.JobMetaManagedBy: "policy:database-controller"})
		must.Eq(t, 400, errCode(t, err))
	}
	// A key that is not listed is refused with the same answer.
	plain := parameterized("plain", nil, "team")
	must.NoError(t, controller.registerJob(plain))
	_, err = dispatch(dispatcher, "plain", mark)
	must.Eq(t, 400, errCode(t, err))
	must.ErrorContains(t, err, "is reserved: a dispatch can't set it")
	children, err := c.s.State().JobsByIDPrefix(nil, "default", "legacy"+structs.DispatchLaunchSuffix)
	must.NoError(t, err)
	must.Nil(t, children.Next())

	// The other keys are the dispatcher's to set, and the child of a marked
	// parent inherits its mark: the parent was checked for it.
	_, err = dispatch(dispatcher, "plain", map[string]string{"team": "shop"})
	must.NoError(t, err)
	err = dispatcher.registerJob(parameterized("marked", mark, "team"))
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	must.NoError(t, controller.registerJob(parameterized("marked", mark, "team")))
	resp, err := dispatch(dispatcher, "marked", map[string]string{"team": "shop"})
	must.NoError(t, err)
	child := c.jobIn("default", resp.DispatchedJobID)
	must.Eq(t, "marked", child.ParentID)
	must.Eq(t, "shop", child.Meta["team"])
	must.True(t, structs.JobOwnedBy(child.Meta, testOwnerGroup, testOwnerKind, "default", "shop"))
}

// The launch of a periodic job writes a child with the mark of its parent,
// whoever forces it.
func TestJobOwnerMark_PeriodicChild(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	controller := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "controller", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
  object "example.com/Database" { capabilities = ["manage-finalizers"] }
}`))
	submitter := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "submitter", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`))

	cron := mock.PeriodicJob()
	cron.ID, cron.Name, cron.Namespace = "cron", "cron", "default"
	cron.Meta = ownerMeta(testOwnerGroup, testOwnerKind, "default", "shop")
	must.Eq(t, 403, errCode(t, submitter.registerJob(cron)))
	must.NoError(t, controller.registerJob(cron))

	var resp structs.PeriodicForceResponse
	must.NoError(t, c.s.RPC("Periodic.Force", &structs.PeriodicForceRequest{
		JobID: cron.ID, WriteRequest: submitter.write("default"),
	}, &resp))
	children, err := c.s.State().JobsByIDPrefix(nil, "default", cron.ID+structs.PeriodicLaunchSuffix)
	must.NoError(t, err)
	raw := children.Next()
	must.NotNil(t, raw, must.Sprint("the child of the marked periodic job was not launched"))
	child := raw.(*structs.Job)
	must.Eq(t, cron.ID, child.ParentID)
	must.True(t, structs.JobOwnedBy(child.Meta, testOwnerGroup, testOwnerKind, "default", "shop"))
}

// A release marks its jobs and its objects as its dependents with the token
// of the caller, who has "write" on the releases of the namespace: an
// ordinary release token installs, upgrades and removes a release with a
// job and an object.
func TestJobOwnerMark_ReleaseApply(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	var kind structs.ObjectKindUpsertResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "ObjectKind.UpsertObjectKind", &structs.ObjectKindUpsertRequest{
		ObjectKind:   &structs.ObjectKind{Group: "test.example.com", Kind: "Widget", Namespaced: true, Schema: testReleaseKindSchema},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token},
	}, &kind))

	token := mock.CreatePolicyAndToken(t, c.srv.State(), 1000, "release", `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
  object "rigger.norca.io/Release" { capabilities = ["read", "write"] }
  object "test.example.com/Widget" { capabilities = ["read", "write"] }
}`)
	root := c.token
	c.token = token.SecretID
	defer func() { c.token = root }()

	resources := func(size int) []*structs.ReleaseResource {
		return []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceJob, Job: testReleaseJob("app-web")},
			{Type: structs.ReleaseResourceObject, Object: &structs.CustomObject{
				Group: "test.example.com", Kind: "Widget", Name: "app-widget", Spec: map[string]any{"size": size},
			}},
		}
	}
	_, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app", Version: "1.0.0"}, Resources: resources(2),
	})
	must.NoError(t, err)
	rel := c.get("default", "app")
	must.NotNil(t, rel)

	job, err := c.srv.State().JobByID(nil, "default", "app-web")
	must.NoError(t, err)
	must.NotNil(t, job)
	must.True(t, structs.JobOwnedBy(job.Meta, structs.ReleaseObjectGroup, structs.ReleaseKind, "default", "app"))
	widget, err := c.srv.State().CustomObjectByName(nil, "test.example.com", "Widget", "default", "app-widget")
	must.NoError(t, err)
	must.NotNil(t, widget)
	must.True(t, structs.ObjectOwnedBy(widget, structs.ReleaseObjectGroup, structs.ReleaseKind, "default", "app"))

	// An upgrade keeps the marks, and the release is removed with what it
	// owns.
	_, err = c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app", Version: "1.1.0"}, Resources: resources(3),
	})
	must.NoError(t, err)
	widget, err = c.srv.State().CustomObjectByName(nil, "test.example.com", "Widget", "default", "app-widget")
	must.NoError(t, err)
	must.True(t, structs.ObjectOwnedBy(widget, structs.ReleaseObjectGroup, structs.ReleaseKind, "default", "app"))
	_, err = c.uninstall("default", "app", "")
	must.NoError(t, err)
	must.Nil(t, c.get("default", "app"))

	// The same token does not mark a job of its own as the dependent of a
	// release of a namespace whose releases it may not write.
	other := mock.Namespace()
	other.Name = "other"
	must.NoError(t, c.srv.State().UpsertNamespaces(1100, []*structs.Namespace{other}))
	squat := markedJob("default", "squat", ownerMeta(structs.ReleaseObjectGroup, structs.ReleaseKind, "other", "theirs"))
	var resp structs.JobRegisterResponse
	err = msgpackrpc.CallWithCodec(c.codec, "Job.Register", &structs.JobRegisterRequest{
		Job: squat, WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token.SecretID},
	}, &resp)
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, `owner rigger.norca.io/Release "theirs" in namespace "other"`)
}
