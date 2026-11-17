// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

func TestBusRedactJob(t *testing.T) {
	ci.Parallel(t)

	job := mock.Job()
	job.Meta = map[string]string{"owner": "team-a"}
	job.Payload = []byte("dispatch-secret")
	tg := job.TaskGroups[0]
	task := tg.Tasks[0]
	task.Env = map[string]string{"DB_PASSWORD": "s3cret"}
	task.Config = map[string]interface{}{"image": "app", "auth": map[string]interface{}{"password": "registry-secret"}}
	task.Templates = []*structs.Template{{EmbeddedTmpl: "password=s3cret", DestPath: "local/app.env"}}
	task.Artifacts = []*structs.TaskArtifact{{
		GetterSource:  "https://user:pass@example.com/app.tgz?token=abc",
		GetterHeaders: map[string]string{"Authorization": "Bearer abc"},
	}}
	tg.Services = []*structs.Service{{Name: "web", Checks: []*structs.ServiceCheck{{
		Name: "http", Header: map[string][]string{"Authorization": {"Basic abc"}}, Body: "secret-body",
	}}}}
	original := job.Copy()

	got := busRedactJob(job)
	must.Eq(t, original, job) // the job of the state is not touched

	must.Nil(t, got.Payload)
	must.Eq(t, map[string]string{"owner": busRedacted}, got.Meta)
	gotTask := got.TaskGroups[0].Tasks[0]
	must.Eq(t, map[string]string{"DB_PASSWORD": busRedacted}, gotTask.Env)
	must.MapEmpty(t, gotTask.Config)
	must.Eq(t, busRedacted, gotTask.Templates[0].EmbeddedTmpl)
	must.Eq(t, "local/app.env", gotTask.Templates[0].DestPath)
	must.Eq(t, "https://"+busRedacted+"@example.com/app.tgz?"+busRedacted, gotTask.Artifacts[0].GetterSource)
	must.Eq(t, map[string]string{"Authorization": busRedacted}, gotTask.Artifacts[0].GetterHeaders)
	check := got.TaskGroups[0].Services[0].Checks[0]
	must.Eq(t, []string{busRedacted}, check.Header["Authorization"])
	must.Eq(t, busRedacted, check.Body)
	// What a consumer needs stays.
	must.Eq(t, job.ID, got.ID)
	must.Eq(t, tg.Count, got.TaskGroups[0].Count)
	must.Eq(t, task.Name, gotTask.Name)
	must.Eq(t, task.Driver, gotTask.Driver)

	raw, err := json.Marshal(got)
	must.NoError(t, err)
	for _, secret := range []string{"s3cret", "registry-secret", "pass@", "token=abc", "Bearer abc", "Basic abc", "secret-body", "dispatch-secret"} {
		must.False(t, strings.Contains(string(raw), secret), must.Sprintf("%q left in the redacted job", secret))
	}
}

func TestBusRedactServiceEvent(t *testing.T) {
	ci.Parallel(t)

	reg := &structs.ServiceRegistration{
		ID: "s1", ServiceName: "web", Namespace: "default", JobID: "web",
		Ready: true, Health: structs.ServiceHealthPassing,
		Checks: []*structs.ServiceRegistrationCheck{
			{Name: "http", Role: "readiness", Status: structs.ServiceHealthPassing, Output: "HTTP 200: {\"token\":\"s3cret\"}"},
			{Name: "tcp", Status: structs.ServiceHealthPassing},
		},
	}
	ev := structs.Event{Topic: structs.TopicService, Payload: &structs.ServiceRegistrationStreamEvent{Service: reg}}

	got, redacted := busRedactEvent(&ev)
	must.True(t, redacted)
	service := got.Payload.(*structs.ServiceRegistrationStreamEvent).Service
	must.Eq(t, busRedacted, service.Checks[0].Output)
	must.Eq(t, "", service.Checks[1].Output)
	must.Eq(t, structs.ServiceHealthPassing, service.Checks[0].Status)
	must.Eq(t, "readiness", service.Checks[0].Role)
	must.True(t, service.Ready)
	must.StrContains(t, reg.Checks[0].Output, "s3cret") // the state's copy is untouched

	// Without outputs nothing is copied or marked.
	reg.Checks[0].Output = ""
	_, redacted = busRedactEvent(&ev)
	must.False(t, redacted)
}

func TestBusEventJob(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		event    structs.Event
		expected string
	}{
		{name: "job", event: structs.Event{Topic: structs.TopicJob, Key: "web"}, expected: "web"},
		{name: "allocation", event: structs.Event{Topic: structs.TopicAllocation, Key: "a1", FilterKeys: []string{"web", "d1"}}, expected: "web"},
		{name: "evaluation", event: structs.Event{Topic: structs.TopicEvaluation, FilterKeys: []string{"web", ""}}, expected: "web"},
		{name: "deployment", event: structs.Event{Topic: structs.TopicDeployment, FilterKeys: []string{"web"}}, expected: "web"},
		{name: "service", event: structs.Event{Topic: structs.TopicService, FilterKeys: []string{"web", "api"}}, expected: "web"},
		{name: "custom object", event: structs.Event{Topic: structs.TopicCustomObject, FilterKeys: []string{"db.example.com/Db"}}},
		{name: "variables", event: structs.Event{Topic: structs.TopicVariables, Key: "nomad/jobs/web"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, busEventJob(&tt.event))
		})
	}
}

// TestConsulKVEndpoint_Notice checks the notice of a written and of a
// deleted key: a message on <prefix>.<region>.<ns>.ConsulKV.<type> with the
// key and without the value, stored in NORCA_KV_CHANGES_<region> and not in
// the event stream.
func TestConsulKVEndpoint_Notice(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	c := &consulKVTest{t: t, srv: s1}
	c.waitBucket("")

	nc, err := s1.bus.Connect()
	must.NoError(t, err)
	defer nc.Close()
	sub, err := nc.SubscribeSync("norca.global.*.ConsulKV.>")
	must.NoError(t, err)
	must.NoError(t, nc.Flush())
	// The leader creates the stream of the notices with the event streams.
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), bus.ConsulKVChangesStreamName("global"))
		return err == nil, err
	})

	ok, err := c.put("", "default", "app/config/level", "debug-SECRET-VALUE", nil)
	must.NoError(t, err)
	must.True(t, ok)
	msg, err := sub.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.Eq(t, "norca.global.default.ConsulKV.KeySet", msg.Subject)
	must.Eq(t, "app/config/level", msg.Header.Get(bus.HeaderKey))
	must.False(t, strings.Contains(string(msg.Data), "SECRET-VALUE"), must.Sprint("the notice carries the value"))
	var ev struct{ Topic, Type, Key, Namespace string }
	must.NoError(t, json.Unmarshal(msg.Data, &ev))
	must.Eq(t, "ConsulKV", ev.Topic)
	must.Eq(t, "KeySet", ev.Type)
	must.Eq(t, "app/config/level", ev.Key)
	must.Eq(t, "default", ev.Namespace)

	// A CAS that fails changes nothing and sends nothing.
	wrong := uint64(1 << 40)
	ok, err = c.put("", "default", "app/config/level", "other", &wrong)
	must.NoError(t, err)
	must.False(t, ok)

	ok, err = c.delete("", "default", "app/config/level", false, nil)
	must.NoError(t, err)
	must.True(t, ok)
	msg, err = sub.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.Eq(t, "norca.global.default.ConsulKV.KeyDeleted", msg.Subject)
	_, err = sub.NextMsg(500 * time.Millisecond)
	must.True(t, errors.Is(err, nats.ErrTimeout), must.Sprintf("an extra notice: %v", err))

	// The notices are stored in their own stream, not in the event stream.
	changes, err := js.Stream(t.Context(), bus.ConsulKVChangesStreamName("global"))
	must.NoError(t, err)
	stored, err := changes.GetLastMsgForSubject(t.Context(), "norca.global.default.ConsulKV.KeyDeleted")
	must.NoError(t, err)
	must.Eq(t, "app/config/level", stored.Header.Get(bus.HeaderKey))
	set, err := changes.GetLastMsgForSubject(t.Context(), "norca.global.default.ConsulKV.KeySet")
	must.NoError(t, err)
	must.StrNotContains(t, string(set.Data), "SECRET-VALUE")
	stream, err := js.Stream(t.Context(), bus.EventStreamName("global"))
	must.NoError(t, err)
	must.Eq(t, []string{"norca.global.*.*"}, stream.CachedInfo().Config.Subjects)
	_, err = stream.GetLastMsgForSubject(t.Context(), "norca.global.default.ConsulKV.>")
	must.True(t, errors.Is(err, jetstream.ErrMsgNotFound), must.Sprintf("a notice in the event stream: %v", err))
}

// TestConsulKVEndpoint_NoticeOnlyForDeletedKeys checks that KeyDeleted is
// only sent for a key that was deleted: a delete of a key that does not
// exist answers true (with a check-and-set index too) and sends nothing.
func TestConsulKVEndpoint_NoticeOnlyForDeletedKeys(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	c := &consulKVTest{t: t, srv: s1}
	c.waitBucket("")

	nc, err := s1.bus.Connect()
	must.NoError(t, err)
	defer nc.Close()
	sub, err := nc.SubscribeSync("norca.global.*.ConsulKV.>")
	must.NoError(t, err)
	must.NoError(t, nc.Flush())
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), bus.ConsulKVChangesStreamName("global"))
		return err == nil, err
	})
	next := func(subject, key string) {
		t.Helper()
		msg, err := sub.NextMsg(5 * time.Second)
		must.NoError(t, err, must.Sprint(subject, key))
		must.Eq(t, subject, msg.Subject)
		must.Eq(t, key, msg.Header.Get(bus.HeaderKey))
	}
	none := func(what string) {
		t.Helper()
		msg, err := sub.NextMsg(500 * time.Millisecond)
		must.True(t, errors.Is(err, nats.ErrTimeout), must.Sprintf("%s: an extra notice: %v %v", what, err, msg))
	}
	const set, deleted = "norca.global.default.ConsulKV.KeySet", "norca.global.default.ConsulKV.KeyDeleted"

	// A key that does not exist: true, and no notice, with and without cas.
	zero, some := uint64(0), uint64(7)
	for name, cas := range map[string]*uint64{"cas 0": &zero, "cas 7": &some, "no cas": nil} {
		ok, err := c.delete("", "default", "app/missing", false, cas)
		must.NoError(t, err, must.Sprint(name))
		must.True(t, ok, must.Sprint(name))
		none("delete of a missing key, " + name)
	}

	// A check-and-set delete that deletes sends the notice; one that does
	// not (cas 0, a stale index) sends none.
	ok, err := c.put("", "default", "app/key", "v", nil)
	must.NoError(t, err)
	must.True(t, ok)
	next(set, "app/key")
	got, err := c.get("", "default", "app/key", false, 0)
	must.NoError(t, err)
	must.Len(t, 1, got.Entries)
	index := got.Entries[0].Revision
	stale := index + 1000
	for name, cas := range map[string]*uint64{"cas 0": &zero, "stale cas": &stale} {
		ok, err = c.delete("", "default", "app/key", false, cas)
		must.NoError(t, err, must.Sprint(name))
		must.False(t, ok, must.Sprint(name))
		none("refused delete, " + name)
	}
	ok, err = c.delete("", "default", "app/key", false, &index)
	must.NoError(t, err)
	must.True(t, ok)
	next(deleted, "app/key")

	// The deleted key is a missing one again.
	ok, err = c.delete("", "default", "app/key", false, &index)
	must.NoError(t, err)
	must.True(t, ok)
	ok, err = c.delete("", "default", "app/key", false, nil)
	must.NoError(t, err)
	must.True(t, ok)
	none("delete of a deleted key")

	// A recursive delete sends a notice for every key it deletes.
	ok, err = c.put("", "default", "tree/a", "v", nil)
	must.NoError(t, err)
	must.True(t, ok)
	next(set, "tree/a")
	ok, err = c.delete("", "default", "tree/", true, nil)
	must.NoError(t, err)
	must.True(t, ok)
	next(deleted, "tree/a")
	ok, err = c.delete("", "default", "tree/", true, nil)
	must.NoError(t, err)
	must.True(t, ok)
	none("recursive delete of nothing")
}
