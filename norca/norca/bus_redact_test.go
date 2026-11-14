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
// deleted key: a live message on <prefix>.<region>.<ns>.ConsulKV.<type> with
// the key and without the value, which the event stream does not keep.
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

	// The event stream does not keep the notices.
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	var stream jetstream.Stream
	waitBus(t, func() (bool, error) {
		stream, err = js.Stream(t.Context(), bus.EventStreamName("global"))
		return err == nil, err
	})
	must.Eq(t, []string{"norca.global.*.*"}, stream.CachedInfo().Config.Subjects)
	_, err = stream.GetLastMsgForSubject(t.Context(), "norca.global.default.ConsulKV.>")
	must.True(t, errors.Is(err, jetstream.ErrMsgNotFound), must.Sprintf("a notice in the event stream: %v", err))
}
