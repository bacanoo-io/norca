// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/objectgc"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func (c *objectClient) deleteWith(ns, name, propagation, token string) (*structs.CustomObjectWriteResponse, error) {
	req := &structs.CustomObjectDeleteRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		Propagation:  propagation,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.CustomObjectWriteResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.Delete", req, &resp)
	return &resp, err
}

func (c *objectClient) finalizers(ns, name string, add, remove []string, token string) (*structs.CustomObjectWriteResponse, error) {
	req := &structs.CustomObjectFinalizersRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		Add:          add,
		Remove:       remove,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.CustomObjectWriteResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.UpdateFinalizers", req, &resp)
	return &resp, err
}

func (c *objectClient) recordEvents(ns, name string, token string, events ...*structs.ObjectEvent) error {
	req := &structs.CustomObjectEventsRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		Events:       events,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.GenericResponse
	return msgpackrpc.CallWithCodec(c.codec, "CustomObject.RecordEvents", req, &resp)
}

func (c *objectClient) events(ns, name, token string) ([]*structs.ObjectEvent, error) {
	req := &structs.CustomObjectSpecificRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.CustomObjectEventsResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.ListEvents", req, &resp)
	return resp.Events, err
}

func (c *objectClient) scale(ns, name string, replicas int64, token string) (*structs.CustomObjectScaleResponse, error) {
	req := &structs.CustomObjectScaleRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		Replicas:     replicas,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.CustomObjectScaleResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.Scale", req, &resp)
	return &resp, err
}

func (c *objectClient) getScale(ns, name, token string) (*structs.ObjectScale, error) {
	req := &structs.CustomObjectSpecificRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.SingleObjectScaleResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.GetScale", req, &resp)
	return resp.Scale, err
}

func TestCustomObjectEndpoint_Finalizers(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	_, err := c.upsertKind(testPGKind(true), false, "")
	must.NoError(t, err)

	const fin = "example.com/cleanup"
	obj := testPGObject("default", "billing", map[string]any{"version": "16"})
	obj.Finalizers = []string{fin}
	resp, err := c.apply(obj, nil, false, "")
	must.NoError(t, err)
	must.Eq(t, []string{fin}, resp.Object.Finalizers)

	// An apply without finalizers keeps them.
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "17"}), nil, false, "")
	must.NoError(t, err)
	stored, err := c.get("default", "billing", "")
	must.NoError(t, err)
	must.Eq(t, []string{fin}, stored.Finalizers)

	// The deletion only marks the object while it has finalizers.
	del, err := c.deleteWith("default", "billing", "", "")
	must.NoError(t, err)
	must.True(t, del.Object.Deleting())
	stored, err = c.get("default", "billing", "")
	must.NoError(t, err)
	must.NotNil(t, stored)
	must.True(t, stored.Deleting())
	must.Eq(t, structs.ObjectPropagationBackground, stored.DeletionPropagation)

	// Deleting again changes nothing; finalizers can't be added anymore,
	// but the spec and the status can still be written.
	del, err = c.deleteWith("default", "billing", structs.ObjectPropagationOrphan, "")
	must.NoError(t, err)
	must.True(t, del.Unchanged)
	_, err = c.finalizers("default", "billing", []string{"example.com/other"}, nil, "")
	must.Eq(t, 409, errCode(t, err))
	_, err = c.status("default", "billing", map[string]any{"readyReplicas": 0}, nil, "")
	must.NoError(t, err)

	// Removing the last finalizer deletes the object.
	_, err = c.finalizers("default", "billing", nil, []string{fin}, "")
	must.NoError(t, err)
	stored, err = c.get("default", "billing", "")
	must.NoError(t, err)
	must.Nil(t, stored)

	// An object without finalizers is deleted at once.
	_, err = c.apply(testPGObject("default", "plain", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	del, err = c.deleteWith("default", "plain", "", "")
	must.NoError(t, err)
	must.False(t, del.Object.Deleting())
	stored, err = c.get("default", "plain", "")
	must.NoError(t, err)
	must.Nil(t, stored)

	// Finalizers are added and removed on the finalizers endpoint.
	_, err = c.apply(testPGObject("default", "f", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	fresp, err := c.finalizers("default", "f", []string{"a.example.com/x", "b.example.com/y"}, nil, "")
	must.NoError(t, err)
	must.Eq(t, []string{"a.example.com/x", "b.example.com/y"}, fresp.Object.Finalizers)
	fresp, err = c.finalizers("default", "f", nil, []string{"a.example.com/x"}, "")
	must.NoError(t, err)
	must.Eq(t, []string{"b.example.com/y"}, fresp.Object.Finalizers)
	_, err = c.finalizers("default", "f", []string{"bad name"}, nil, "")
	must.Eq(t, 400, errCode(t, err))
	_, err = c.finalizers("default", "missing", []string{"a.example.com/x"}, nil, "")
	must.Eq(t, 404, errCode(t, err))
}

// TestCustomObjectEndpoint_DeletionModes is the acceptance test of the
// deletion modes: with foreground deletion the object disappears only after
// the jobs it owns are deleted; with orphan deletion the jobs remain.
func TestCustomObjectEndpoint_DeletionModes(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)
	s.objectGC.SetConfig(&objectgc.Config{
		Interval:           50 * time.Millisecond,
		IdleInterval:       time.Second,
		TombstoneRetention: time.Second,
	})

	_, err := c.upsertKind(testPGKind(true), false, "")
	must.NoError(t, err)

	ownedJob := func(id, owner string) {
		job := mock.Job()
		job.ID = id
		// No placements: the job's evaluations complete without nodes.
		job.TaskGroups[0].Count = 0
		job.Meta = map[string]string{
			structs.JobMetaOwner:     structs.ObjectOwnerMetaValue("db.example.com", "PostgresCluster", "default", owner),
			structs.JobMetaOwnerKind: "db.example.com/PostgresCluster",
		}
		req := &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Job.Register", req, &resp))
	}
	getJob := func(id string) *structs.Job {
		job, err := s.State().JobByID(nil, "default", id)
		must.NoError(t, err)
		return job
	}
	owned := func(name, owner string) {
		obj := testPGObject("default", name, map[string]any{"version": "16"})
		obj.Owners = []*structs.ObjectOwnerReference{{Kind: "db.example.com/PostgresCluster", Name: owner}}
		resp, err := c.apply(obj, nil, false, "")
		must.NoError(t, err)
		// The namespace and the ID of the owner are filled in.
		must.Eq(t, "default", resp.Object.Owners[0].Namespace)
		ownerObj, err := c.get("default", owner, "")
		must.NoError(t, err)
		must.Eq(t, ownerObj.ID, resp.Object.Owners[0].ID)
	}
	exists := func(name string) bool {
		obj, err := c.get("default", name, "")
		must.NoError(t, err)
		return obj != nil
	}

	// Foreground: the owner stays until its jobs are stopped and dead and
	// its dependent objects are gone.
	_, err = c.apply(testPGObject("default", "fg", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	owned("fg-child", "fg")
	ownedJob("fg-job", "fg")

	del, err := c.deleteWith("default", "fg", structs.ObjectPropagationForeground, "")
	must.NoError(t, err)
	must.True(t, del.Object.Deleting())
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			if exists("fg") {
				// While the owner exists, its job may not be gone yet; once
				// it is gone, the job must be.
				return errNotYet
			}
			job := getJob("fg-job")
			if !job.Stop || job.Status != structs.JobStatusDead {
				t.Fatalf("owner removed before its job was deleted: stop=%v status=%s", job.Stop, job.Status)
			}
			if exists("fg-child") {
				t.Fatal("owner removed before its dependent object")
			}
			return nil
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	))

	// Orphan: the object disappears at once and its job remains.
	_, err = c.apply(testPGObject("default", "orph", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	owned("orph-child", "orph")
	ownedJob("orph-job", "orph")
	del, err = c.deleteWith("default", "orph", structs.ObjectPropagationOrphan, "")
	must.NoError(t, err)
	must.False(t, del.Object.Deleting())
	must.False(t, exists("orph"))

	// Background: the object disappears at once and its job and dependent
	// object afterwards.
	_, err = c.apply(testPGObject("default", "bg", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	owned("bg-child", "bg")
	ownedJob("bg-job", "bg")
	_, err = c.deleteWith("default", "bg", "", "")
	must.NoError(t, err)
	must.False(t, exists("bg"))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return getJob("bg-job").Stop && !exists("bg-child") }),
		wait.Timeout(30*time.Second),
		wait.Gap(50*time.Millisecond),
	))

	// The orphaned job and object were left alone meanwhile.
	must.False(t, getJob("orph-job").Stop)
	must.True(t, exists("orph-child"))
}

var errNotYet = structs.NewErrRPCCoded(500, "not yet")

func TestCustomObjectEndpoint_EventsAndScale(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	kind := testPGKind(true)
	kind.Scale = &structs.ObjectKindScale{SpecPath: "spec.replicas", StatusPath: "status.readyReplicas"}
	_, err := c.upsertKind(kind, false, "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 2}), nil, false, "")
	must.NoError(t, err)

	// Events: repetitions are counted, invalid events and missing objects
	// are rejected.
	ev := &structs.ObjectEvent{Reason: "Reconciled", Message: "all good"}
	must.NoError(t, c.recordEvents("default", "billing", "", ev))
	must.NoError(t, c.recordEvents("default", "billing", "", ev))
	must.NoError(t, c.recordEvents("default", "billing", "", &structs.ObjectEvent{Type: structs.ObjectEventTypeWarning, Reason: "BackupFailed"}))
	events, err := c.events("default", "billing", "")
	must.NoError(t, err)
	must.Len(t, 2, events)
	must.Eq(t, 2, events[0].Count)
	must.Eq(t, structs.ObjectEventTypeNormal, events[0].Type)
	must.Eq(t, "anonymous", events[0].Reporter)
	must.Eq(t, structs.ObjectEventTypeWarning, events[1].Type)

	must.Eq(t, 400, errCode(t, c.recordEvents("default", "billing", "", &structs.ObjectEvent{Reason: "not a reason"})))
	must.Eq(t, 400, errCode(t, c.recordEvents("default", "billing", "")))
	must.Eq(t, 404, errCode(t, c.recordEvents("default", "missing", "", ev)))

	// Scale: the spec path, the status path and the event log.
	scale, err := c.getScale("default", "billing", "")
	must.NoError(t, err)
	must.Eq(t, int64(2), scale.Replicas)
	must.Nil(t, scale.StatusReplicas)
	_, err = c.status("default", "billing", map[string]any{"readyReplicas": 2}, nil, "")
	must.NoError(t, err)
	scale, err = c.getScale("default", "billing", "")
	must.NoError(t, err)
	must.Eq(t, pointer.Of(int64(2)), scale.StatusReplicas)

	before, err := c.get("default", "billing", "")
	must.NoError(t, err)
	sresp, err := c.scale("default", "billing", 5, "")
	must.NoError(t, err)
	must.Eq(t, int64(5), sresp.Scale.Replicas)
	must.Eq(t, before.Generation+1, sresp.Scale.Generation)
	after, err := c.get("default", "billing", "")
	must.NoError(t, err)
	must.Eq[any](t, float64(5), after.Spec["replicas"])
	must.Eq(t, before.Status, after.Status)

	events, err = c.events("default", "billing", "")
	must.NoError(t, err)
	last := events[len(events)-1]
	must.Eq(t, structs.ObjectScaleReason, last.Reason)
	must.Eq(t, "5", last.Meta[structs.ObjectEventMetaCount])
	must.Eq(t, "2", last.Meta[structs.ObjectEventMetaPreviousCount])

	// The same count changes nothing; the kind's rules apply.
	sresp, err = c.scale("default", "billing", 5, "")
	must.NoError(t, err)
	must.True(t, sresp.Unchanged)
	events2, err := c.events("default", "billing", "")
	must.NoError(t, err)
	must.Eq(t, len(events), len(events2))
	_, err = c.scale("default", "billing", 9, "")
	must.ErrorContains(t, err, "between 1 and 7")
	_, err = c.scale("default", "billing", -1, "")
	must.Eq(t, 400, errCode(t, err))

	// A kind without a scale subresource.
	_, err = c.upsertKind(testPGKind(true), false, "")
	must.NoError(t, err)
	_, err = c.scale("default", "billing", 3, "")
	must.Eq(t, 400, errCode(t, err))
	_, err = c.getScale("default", "billing", "")
	must.Eq(t, 400, errCode(t, err))

	// The log is deleted with the object.
	_, err = c.deleteWith("default", "billing", "", "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	events, err = c.events("default", "billing", "")
	must.NoError(t, err)
	must.Len(t, 0, events)
}

func TestCustomObjectEndpoint_LifecycleACL(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	user := mock.CreatePolicyAndToken(t, s.State(), 1001, "user", `
namespace "default" { object "db.example.com/*" { capabilities = ["read", "write"] } }`)
	controller := mock.CreatePolicyAndToken(t, s.State(), 1003, "controller", `
namespace "*" {
  object "db.example.com/*" { capabilities = ["read", "write-status", "write-events", "manage-finalizers"] }
}`)
	gitops := mock.CreatePolicyAndToken(t, s.State(), 1005, "gitops", `
namespace "default" { object "db.example.com/*" { policy = "write" } }`)

	kind := testPGKind(true)
	kind.Scale = &structs.ObjectKindScale{SpecPath: "spec.replicas"}
	_, err := c.upsertKind(kind, false, root.SecretID)
	must.NoError(t, err)

	// Adding finalizers requires manage-finalizers, also on apply.
	obj := testPGObject("default", "billing", map[string]any{"version": "16"})
	obj.Finalizers = []string{"example.com/cleanup"}
	_, err = c.apply(obj, nil, false, user.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	obj.Finalizers = nil
	_, err = c.apply(obj, nil, false, user.SecretID)
	must.NoError(t, err)
	_, err = c.finalizers("default", "billing", []string{"example.com/cleanup"}, nil, user.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.finalizers("default", "billing", []string{"example.com/cleanup"}, nil, controller.SecretID)
	must.NoError(t, err)

	// Events require write-events; reading them read.
	ev := &structs.ObjectEvent{Reason: "Reconciled"}
	must.EqError(t, c.recordEvents("default", "billing", user.SecretID, ev), structs.ErrPermissionDenied.Error())
	must.NoError(t, c.recordEvents("default", "billing", controller.SecretID, ev))
	events, err := c.events("default", "billing", user.SecretID)
	must.NoError(t, err)
	must.Len(t, 1, events)
	reporter := controller.Name
	if reporter == "" {
		reporter = structs.ManagedByTokenPrefix + controller.AccessorID
	}
	must.Eq(t, reporter, events[0].Reporter)

	// An object managed by an identity can only be changed by it (and by
	// management tokens); the scale subresource and finalizers are exempt.
	managed := testPGObject("default", "managed", map[string]any{"version": "16"})
	managed.ManagedBy = structs.ManagedByPolicyPrefix + "gitops"
	_, err = c.apply(managed, nil, false, gitops.SecretID)
	must.NoError(t, err)

	managed.Spec = map[string]any{"version": "17"}
	_, err = c.apply(managed, nil, false, user.SecretID)
	must.ErrorContains(t, err, "managed by")
	must.Eq(t, 403, errCode(t, err))
	_, err = c.deleteWith("default", "managed", "", user.SecretID)
	must.ErrorContains(t, err, "managed by")
	_, err = c.apply(managed, nil, false, gitops.SecretID)
	must.NoError(t, err)
	_, err = c.apply(managed, nil, false, root.SecretID)
	must.NoError(t, err)
	_, err = c.scale("default", "managed", 3, user.SecretID)
	must.NoError(t, err)
	_, err = c.finalizers("default", "managed", []string{"example.com/cleanup"}, nil, controller.SecretID)
	must.NoError(t, err)

	// The managing identity can hand the object over.
	managed.ManagedBy = "free-form"
	_, err = c.apply(managed, nil, false, gitops.SecretID)
	must.NoError(t, err)
	managed.Spec = map[string]any{"version": "18"}
	_, err = c.apply(managed, nil, false, user.SecretID)
	must.NoError(t, err)
}

func TestJobEndpoint_ManagedJob(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	ctrl := mock.CreatePolicyAndToken(t, s.State(), 1001, "ctrl", `namespace "default" { policy = "write" }`)
	other := mock.CreatePolicyAndToken(t, s.State(), 1003, "other", `namespace "default" { policy = "write" }`)

	register := func(job *structs.Job, token string) error {
		req := &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: token},
		}
		var resp structs.JobRegisterResponse
		return msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
	}
	deregister := func(id, token string) error {
		req := &structs.JobDeregisterRequest{
			JobID:        id,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token},
		}
		var resp structs.JobDeregisterResponse
		return msgpackrpc.CallWithCodec(codec, "Job.Deregister", req, &resp)
	}
	scale := func(job *structs.Job, count int64, token string) error {
		req := &structs.JobScaleRequest{
			JobID:        job.ID,
			Target:       map[string]string{structs.ScalingTargetGroup: job.TaskGroups[0].Name},
			Count:        pointer.Of(count),
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token},
		}
		var resp structs.JobRegisterResponse
		return msgpackrpc.CallWithCodec(codec, "Job.Scale", req, &resp)
	}

	job := mock.Job()
	job.Meta = map[string]string{structs.JobMetaManagedBy: structs.ManagedByPolicyPrefix + "ctrl"}
	must.NoError(t, register(job.Copy(), ctrl.SecretID))

	// Others can't update, scale or stop the managed job.
	update := job.Copy()
	update.TaskGroups[0].Count = 3
	err := register(update, other.SecretID)
	must.ErrorContains(t, err, "managed by")
	must.ErrorContains(t, scale(job, 3, other.SecretID), "managed by")
	must.ErrorContains(t, deregister(job.ID, other.SecretID), "managed by")

	// The managing identity and management tokens can.
	must.NoError(t, register(update, ctrl.SecretID))
	must.NoError(t, scale(job, 2, ctrl.SecretID))
	must.NoError(t, scale(job, 4, root.SecretID))

	// A group with its own scaling block belongs to the autoscaler: others
	// may scale it.
	scaled, _ := mock.JobWithScalingPolicy()
	scaled.TaskGroups[0].Scaling.Min = 1
	scaled.TaskGroups[0].Scaling.Max = 10
	scaled.Meta = map[string]string{structs.JobMetaManagedBy: structs.ManagedByPolicyPrefix + "ctrl"}
	must.NoError(t, s.State().UpsertJob(structs.MsgTypeTestSetup, 2000, nil, scaled))
	must.NoError(t, scale(scaled, 5, other.SecretID))

	// Jobs whose managed_by is not an identity are not protected.
	free := mock.Job()
	free.Meta = map[string]string{structs.JobMetaManagedBy: "some-tool"}
	must.NoError(t, register(free.Copy(), ctrl.SecretID))
	must.NoError(t, deregister(free.ID, other.SecretID))

	must.NoError(t, deregister(job.ID, ctrl.SecretID))
}
