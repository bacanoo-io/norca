// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strconv"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// TestJobScale_AutoscalerIsItsIdentity checks that Job.Scale takes a request
// for the built-in autoscaler's own by the identity that sends it, not by
// the meta of the request: anyone with scale-job could send that meta, and
// with it scale up during a deployment and write events the autoscaler
// reads as its own.
func TestJobScale_AutoscalerIsItsIdentity(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, testAutoscalerConfig)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)
	store := s1.fsm.State()

	// A policy of the built-in engine that wants the count the group has,
	// so the autoscaler itself does nothing here.
	job, _ := mock.JobWithScalingPolicy()
	group := job.TaskGroups[0]
	group.Count = 2
	group.Scaling.Min, group.Scaling.Max = 1, 6
	group.Scaling.Engine = structs.ScalingPolicyEngineBuiltin
	group.Scaling.Policy = fixedValuePolicy(2)
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{Job: job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: root.SecretID}}, &regResp))
	stored, err := store.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)

	// An active deployment of the job.
	deployment := mock.Deployment()
	deployment.Namespace = job.Namespace
	deployment.JobID = job.ID
	deployment.JobVersion = stored.Version
	deployment.JobModifyIndex = stored.JobModifyIndex
	deployment.JobCreateIndex = stored.CreateIndex
	deployment.TaskGroups = map[string]*structs.DeploymentState{group.Name: {DesiredTotal: 2}}
	index, _ := store.LatestIndex()
	must.NoError(t, store.UpsertDeployment(index+1, deployment))

	scaler := mock.CreatePolicyAndToken(t, store, index+2, "scaler", `
namespace "default" { capabilities = ["scale-job", "read-job"] }`)

	ownMeta := func() map[string]interface{} {
		return map[string]interface{}{"nomad_autoscaler.engine": structs.ScalingPolicyEngineBuiltin}
	}
	request := func(count int64, meta map[string]interface{}) *structs.JobScaleRequest {
		return &structs.JobScaleRequest{
			JobID:   job.ID,
			Target:  map[string]string{structs.ScalingTargetGroup: group.Name},
			Count:   pointer.Of(count),
			Message: "test",
			Meta:    meta,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace,
				AuthToken: scaler.SecretID},
		}
	}
	count := func() int {
		out, err := store.JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		return out.TaskGroups[0].Count
	}

	// scale-job with the autoscaler's meta: not the autoscaler.
	var resp structs.JobRegisterResponse
	err = msgpackrpc.CallWithCodec(codec, "Job.Scale", request(3, ownMeta()), &resp)
	must.Error(t, err, must.Sprint("scale-job passed for the built-in autoscaler with its meta"))
	must.ErrorContains(t, err, "reserved for the built-in autoscaler")
	must.Eq(t, 2, count())
	// The same for the meta of an oscillation suspension.
	suspend := request(3, map[string]interface{}{
		"nomad_autoscaler.suspended_until": strconv.FormatInt(time.Now().Add(24*time.Hour).UnixNano(), 10)})
	suspend.Count, suspend.Error = nil, true
	err = msgpackrpc.CallWithCodec(codec, "Job.Scale", suspend, &resp)
	must.ErrorContains(t, err, "reserved for the built-in autoscaler")
	// Without the meta the deployment blocks the change, as for anyone.
	err = msgpackrpc.CallWithCodec(codec, "Job.Scale", request(3, nil), &resp)
	must.ErrorContains(t, err, "job scaling blocked due to active deployment")
	must.Eq(t, 2, count())
	events, _, err := store.ScalingEventsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 0, events[group.Name])

	// The autoscaler's identity scales up during the deployment, without
	// the warning of a change by hand.
	own := request(3, ownMeta())
	own.AuthToken = ""
	reply, err := (&autoscalerJobScaler{srv: s1}).Scale(own)
	must.NoError(t, err)
	must.Eq(t, "", reply.Warnings)
	must.Eq(t, 3, count())

	// After the deployment a holder of scale-job changes the count, and is
	// told that the policy decides it.
	done := deployment.Copy()
	done.Status = structs.DeploymentStatusSuccessful
	index, _ = store.LatestIndex()
	must.NoError(t, store.UpsertDeployment(index+1, done))
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Scale", request(4, nil), &resp))
	must.Eq(t, scaledByPolicyWarning, resp.Warnings)
	must.Eq(t, 4, count())
}

// TestCustomObjectEndpoint_AutoscalerEventsAreReserved checks that the
// entries of an object's event log the autoscaler reads (the "Scaled"
// reason with the counts, which the scale subresource writes, and the meta
// of its engine and of an oscillation suspension) can't be written with
// write-events or with the scale subresource by someone else, and that the
// event log of the built-in kinds is not written through this endpoint.
func TestCustomObjectEndpoint_AutoscalerEventsAreReserved(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	reporter := mock.CreatePolicyAndToken(t, s.State(), 1001, "reporter", `
namespace "default" { object "db.example.com/*" { capabilities = ["read", "write-events"] } }
namespace "default" { object "packs.norca.io/*" { capabilities = ["read", "write-events"] } }`)
	writer := mock.CreatePolicyAndToken(t, s.State(), 1003, "writer", `
namespace "default" { object "db.example.com/*" { capabilities = ["read", "write"] } }`)

	kind := testPGKind(true)
	kind.Scale = &structs.ObjectKindScale{SpecPath: "spec.replicas"}
	_, err := c.upsertKind(kind, false, root.SecretID)
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 2}), nil, false, root.SecretID)
	must.NoError(t, err)

	until := strconv.FormatInt(time.Now().Add(24*time.Hour).UnixNano(), 10)
	forged := []*structs.ObjectEvent{
		{Reason: structs.ObjectScaleReason, Message: "scaled from 1 to 5",
			Meta: map[string]string{structs.ObjectEventMetaCount: "5", structs.ObjectEventMetaPreviousCount: "1"}},
		{Type: structs.ObjectEventTypeWarning, Reason: "Suspended",
			Meta: map[string]string{"nomad_autoscaler.suspended_until": until}},
		{Reason: "Reconciled", Meta: map[string]string{"nomad_autoscaler.engine": structs.ScalingPolicyEngineBuiltin}},
	}
	for _, ev := range forged {
		err := c.recordEvents("default", "billing", reporter.SecretID, ev)
		must.Error(t, err, must.Sprintf("write-events wrote the reserved event %s %v", ev.Reason, ev.Meta))
		must.Eq(t, 400, errCode(t, err))
		must.ErrorContains(t, err, "reserved")
		// A management token is not the autoscaler either.
		must.Eq(t, 400, errCode(t, c.recordEvents("default", "billing", root.SecretID, ev)))
	}
	// The scale subresource writes the "Scaled" event itself; its meta is
	// the caller's, without the autoscaler's keys.
	req := &structs.CustomObjectScaleRequest{
		Group: "db.example.com", Kind: "PostgresCluster", Name: "billing", Replicas: 3,
		Meta:         map[string]string{"nomad_autoscaler.engine": structs.ScalingPolicyEngineBuiltin},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: writer.SecretID},
	}
	var scaleResp structs.CustomObjectScaleResponse
	err = msgpackrpc.CallWithCodec(c.codec, "CustomObject.Scale", req, &scaleResp)
	must.Eq(t, 400, errCode(t, err))
	must.ErrorContains(t, err, "reserved")

	// Nothing the autoscaler reads was written.
	events, err := c.events("default", "billing", root.SecretID)
	must.NoError(t, err)
	must.Len(t, 0, events)

	// An ordinary event and an ordinary scaling are written.
	must.NoError(t, c.recordEvents("default", "billing", reporter.SecretID,
		&structs.ObjectEvent{Reason: "Reconciled", Meta: map[string]string{"attempt": "1"}}))
	_, err = c.scale("default", "billing", 3, writer.SecretID)
	must.NoError(t, err)

	// The autoscaler's identity writes its events: the scaling with its
	// engine, and the suspension.
	as := &autoscalerJobScaler{srv: s}
	_, err = as.ScaleObject(&structs.CustomObjectScaleRequest{
		Group: "db.example.com", Kind: "PostgresCluster", Name: "billing", Replicas: 4,
		Meta:         map[string]string{"nomad_autoscaler.engine": structs.ScalingPolicyEngineBuiltin},
		WriteRequest: structs.WriteRequest{Namespace: "default"},
	})
	must.NoError(t, err)
	must.NoError(t, as.RecordObjectEvents(&structs.CustomObjectEventsRequest{
		Group: "db.example.com", Kind: "PostgresCluster", Name: "billing",
		Events:       []*structs.ObjectEvent{forged[1]},
		WriteRequest: structs.WriteRequest{Namespace: "default"},
	}))
	events, err = c.events("default", "billing", root.SecretID)
	must.NoError(t, err)
	must.Len(t, 4, events)
	must.Eq(t, structs.ObjectScaleReason, events[2].Reason)
	must.Eq(t, structs.ScalingPolicyEngineBuiltin, events[2].Meta["nomad_autoscaler.engine"])
	must.Eq(t, until, events[3].Meta["nomad_autoscaler.suspended_until"])

	// The event log of a built-in kind is the release API's.
	err = msgpackrpc.CallWithCodec(c.codec, "CustomObject.RecordEvents", &structs.CustomObjectEventsRequest{
		Group: structs.ReleaseObjectGroup, Kind: "Release", Name: "app",
		Events:       []*structs.ObjectEvent{{Reason: "Reconciled"}},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: reporter.SecretID},
	}, &structs.GenericResponse{})
	must.Eq(t, 400, errCode(t, err))
	must.ErrorContains(t, err, "built in")
}
