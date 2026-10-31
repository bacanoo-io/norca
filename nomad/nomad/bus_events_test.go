// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// busJS opens a JetStream context on the server's bus.
func busJS(t *testing.T, s *Server) jetstream.JetStream {
	t.Helper()
	nc, err := s.bus.Connect()
	must.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	return js
}

// lastJobEvent returns the newest message of the Job topic in the event
// stream, waiting until there is one for jobID.
func lastJobEvent(t *testing.T, js jetstream.JetStream, jobID string) *jetstream.RawStreamMsg {
	t.Helper()
	var msg *jetstream.RawStreamMsg
	waitBus(t, func() (bool, error) {
		stream, err := js.Stream(t.Context(), bus.EventStreamName("global"))
		if err != nil {
			return false, err
		}
		m, err := stream.GetLastMsgForSubject(t.Context(), "nomad.global.default.Job")
		if err != nil {
			return false, err
		}
		if m.Header.Get(bus.HeaderKey) != jobID {
			return false, fmt.Errorf("last job event is for %q", m.Header.Get(bus.HeaderKey))
		}
		msg = m
		return true, nil
	})
	return msg
}

func TestBusEvents_Publish(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js := busJS(t, s1)

	// A registered job shows up in the event stream: subject, headers and
	// the event JSON with the region.
	job := mock.Job()
	codec := rpcClient(t, s1)
	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}, &resp))

	msg := lastJobEvent(t, js, job.ID)
	must.Eq(t, "nomad.global."+job.Namespace+".Job", msg.Subject)
	must.Eq(t, "global", msg.Header.Get(bus.HeaderRegion))
	must.Eq(t, "Job", msg.Header.Get(bus.HeaderTopic))
	must.Eq(t, "JobRegistered", msg.Header.Get(bus.HeaderType))
	must.Eq(t, job.Namespace, msg.Header.Get(bus.HeaderNamespace))
	index, err := strconv.ParseUint(msg.Header.Get(bus.HeaderIndex), 10, 64)
	must.NoError(t, err)
	must.Eq(t, resp.JobModifyIndex, index)
	must.Eq(t, fmt.Sprintf("global:%d:Job:JobRegistered:%s:%s", index, job.Namespace, job.ID), msg.Header.Get(bus.HeaderMsgID))

	var ev struct {
		Region  string
		Topic   string
		Type    string
		Key     string
		Index   uint64
		Payload struct{ Job struct{ ID string } }
	}
	must.NoError(t, json.Unmarshal(msg.Data, &ev))
	must.Eq(t, "global", ev.Region)
	must.Eq(t, "Job", ev.Topic)
	must.Eq(t, job.ID, ev.Key)
	must.Eq(t, index, ev.Index)
	must.Eq(t, job.ID, ev.Payload.Job.ID)

	// The stream has the configured limits.
	stream, err := js.Stream(t.Context(), bus.EventStreamName("global"))
	must.NoError(t, err)
	cfg := stream.CachedInfo().Config
	must.Eq(t, []string{"nomad.global.>"}, cfg.Subjects)
	must.Eq(t, bus.DefaultEventsMaxAge, cfg.MaxAge)
	must.Eq(t, int64(16<<20), cfg.MaxBytes)
	must.Eq(t, 2*time.Minute, cfg.Duplicates)
	must.Eq(t, jetstream.S2Compression, cfg.Compression)
	must.Eq(t, 1, cfg.Replicas)

	// The bus status lists the stream.
	st, err := s1.busStatus()
	must.NoError(t, err)
	var found bool
	for _, sd := range st.JetStream.StreamDetails {
		if sd.Name == bus.EventStreamName("global") {
			found = true
			must.Positive(t, sd.Messages)
			must.Eq(t, bus.DefaultEventsMaxAge, sd.MaxAge)
			must.Eq(t, "S2", sd.Compression)
		}
	}
	must.True(t, found)
}

// TestBusEvents_Resume checks that a new publisher continues after the
// newest event of the stream: nothing is lost or published twice.
func TestBusEvents_Resume(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js := busJS(t, s1)
	codec := rpcClient(t, s1)

	register := func() *structs.Job {
		job := mock.Job()
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}, &resp))
		return job
	}
	lastJobEvent(t, js, register().ID)

	// A second publisher (as after a leader change) resumes after the
	// newest event; the running one keeps publishing too, and the stream
	// drops the duplicates by their message ID.
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s1.busEventsLeader(stopCh)
		close(done)
	}()
	job2 := register()
	lastJobEvent(t, js, job2.ID)
	close(stopCh)
	<-done

	stream, err := js.Stream(t.Context(), bus.EventStreamName("global"))
	must.NoError(t, err)
	seen := map[string]bool{}
	info, err := stream.Info(t.Context())
	must.NoError(t, err)
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		m, err := stream.GetMsg(t.Context(), seq)
		must.NoError(t, err)
		id := m.Header.Get(bus.HeaderMsgID)
		must.False(t, seen[id], must.Sprintf("event %s stored twice", id))
		seen[id] = true
	}
}

// TestBusEvents_PushNotice checks that a push notice syncs the registries
// of its source.
func TestBusEvents_PushNotice(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	codec := rpcClient(t, s1)

	var reg PackRegistryResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Upsert", &PackRegistryUpsertRequest{
		Registry:     &PackRegistry{Name: "local", Spec: &PackRegistrySpec{Type: "dir", Source: "builtin"}},
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &reg))
	must.NotNil(t, reg.Registry.Status)
	generation := reg.Registry.Status.SyncGeneration

	notify := func(req *PackPushNoticeRequest) error {
		req.WriteRequest = structs.WriteRequest{Region: "global"}
		var out PackPushNoticeResponse
		return msgpackrpc.CallWithCodec(codec, "Packs.NotifyPush", req, &out)
	}
	must.ErrorContains(t, notify(&PackPushNoticeRequest{}), "needs a Source or a Registry")
	must.NoError(t, notify(&PackPushNoticeRequest{Source: "builtin/", Pack: "redis"}))

	waitBus(t, func() (bool, error) {
		var out PackRegistryListResponse
		if err := msgpackrpc.CallWithCodec(codec, "Packs.List", &PackRegistryListRequest{
			QueryOptions: structs.QueryOptions{Region: "global"},
		}, &out); err != nil {
			return false, err
		}
		for _, r := range out.Registries {
			if r.Name == "local" && r.Status != nil && r.Status.SyncGeneration > generation {
				return true, nil
			}
		}
		return false, fmt.Errorf("the registry was not synced on the push notice")
	})
}
