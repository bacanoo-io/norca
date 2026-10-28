// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeRegistries is a RegistryAPI of fixed registries that records syncs.
type fakeRegistries struct {
	mu     sync.Mutex
	regs   []*Registry
	synced []string
}

func (f *fakeRegistries) Registries(context.Context) ([]*Registry, error) {
	return f.regs, nil
}

func (f *fakeRegistries) Sync(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced = append(f.synced, name)
	return nil
}

func (f *fakeRegistries) syncs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.synced...)
}

func registry(t *testing.T, name, typ, source, digest string) *Registry {
	var r Registry
	must.NoError(t, json.Unmarshal([]byte(`{"Name":"`+name+`","Spec":{"type":"`+typ+`","source":"`+source+`"},
		"Status":{"phase":"Ready","packs":[{"Name":"redis","Version":"1.0.0","Digest":"`+digest+`"}]}}`), &r))
	return &r
}

func registryEventJSON(region, typ, name, kindType, source, digest string) []byte {
	return []byte(`{"Region":"` + region + `","Topic":"CustomObject","Type":"` + typ + `","Key":"packs.norca.io/Registry/` + name + `",
		"Payload":{"Object":{"Group":"packs.norca.io","Kind":"Registry","Name":"` + name + `",
		"Spec":{"type":"` + kindType + `","source":"` + source + `"},
		"Status":{"phase":"Ready","packs":[{"Name":"redis","Version":"1.0.0","Digest":"` + digest + `"}]}}}}`)
}

func TestSyncer_HandlePush(t *testing.T) {
	ctx := context.Background()
	api := &fakeRegistries{regs: []*Registry{
		registry(t, "apps", "oci", "oci://ghcr.io/org/packs", "a"),
		registry(t, "mirror", "git", "https://example.com/packs.git", "a"),
		registry(t, "other", "oci", "oci://ghcr.io/org/other", "a"),
	}}
	s := &Syncer{API: api, Region: "eu", Cooldown: time.Minute, Logger: testLogger(t)}

	synced, err := s.HandlePush(ctx, &PushNotice{Source: "OCI://ghcr.io/org/packs/"})
	must.NoError(t, err)
	must.Eq(t, []string{"apps"}, synced, must.Sprint("sources compare without scheme case and trailing slash"))

	synced, err = s.HandlePush(ctx, &PushNotice{Source: "https://example.com/packs"})
	must.NoError(t, err)
	must.Eq(t, []string{"mirror"}, synced, must.Sprint(".git is optional"))

	synced, err = s.HandlePush(ctx, &PushNotice{Registry: "other"})
	must.NoError(t, err)
	must.Eq(t, []string{"other"}, synced)

	// Within the cooldown a registry is not synced again.
	synced, err = s.HandlePush(ctx, &PushNotice{Registry: "other"})
	must.NoError(t, err)
	must.SliceEmpty(t, synced)
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	synced, _ = s.HandlePush(ctx, &PushNotice{Registry: "other"})
	must.Eq(t, []string{"other"}, synced)

	_, err = s.HandlePush(ctx, &PushNotice{})
	must.Error(t, err)
}

func TestSyncer_HandleRegistryEvent(t *testing.T) {
	ctx := context.Background()
	api := &fakeRegistries{regs: []*Registry{
		registry(t, "apps", "oci", "oci://ghcr.io/org/packs", "old"),
		registry(t, "local", "dir", "/srv/packs", "x"),
	}}
	s := &Syncer{API: api, Region: "eu", Logger: testLogger(t)}

	cases := []struct {
		name  string
		event []byte
		sync  bool
	}{
		{"own region", registryEventJSON("eu", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/org/packs", "new"), false},
		{"same packs", registryEventJSON("us", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/org/packs", "old"), false},
		{"other source", registryEventJSON("us", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/else/packs", "new"), false},
		{"other type", registryEventJSON("us", "CustomObjectStatusUpdated", "local", "bundle", "/srv/packs", "y"), false},
		{"not here", registryEventJSON("us", "CustomObjectUpserted", "missing", "oci", "oci://ghcr.io/org/packs", "new"), false},
		{"deleted", registryEventJSON("us", "CustomObjectDeleted", "apps", "oci", "oci://ghcr.io/org/packs", "new"), false},
		{"new packs", registryEventJSON("us", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/org/packs", "new"), true},
	}
	for _, c := range cases {
		synced, err := s.HandleRegistryEvent(ctx, c.event)
		must.NoError(t, err, must.Sprint(c.name))
		must.Eq(t, c.sync, synced, must.Sprint(c.name))
	}
	must.Eq(t, []string{"apps"}, api.syncs())

	// Other kinds are ignored.
	synced, err := s.HandleRegistryEvent(ctx, []byte(`{"Region":"us","Type":"CustomObjectUpserted","Payload":{"Object":{"Group":"db.example.com","Kind":"Cluster","Name":"apps"}}}`))
	must.NoError(t, err)
	must.False(t, synced)
}

func TestSyncer_SubscribeAndNotifyPush(t *testing.T) {
	srv := runNATS(t, false)
	api := &fakeRegistries{regs: []*Registry{registry(t, "apps", "oci", "oci://ghcr.io/org/packs", "old")}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two bridge instances of the region: the queue group delivers each
	// message to one of them.
	for i := 0; i < 2; i++ {
		s := &Syncer{API: api, Region: "eu", Logger: testLogger(t)}
		_, err := s.Subscribe(ctx, connect(t, srv), "norca.events", true, true)
		must.NoError(t, err)
	}

	pub := connect(t, srv)
	must.NoError(t, PublishPush(pub, "norca.events", &PushNotice{Source: "oci://ghcr.io/org/packs", Pack: "redis", Version: "1.1.0"}))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool { return len(api.syncs()) == 1 }),
		wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))

	must.NoError(t, pub.Publish("norca.events.us.CustomObject",
		registryEventJSON("us", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/org/packs", "new")))
	must.NoError(t, pub.Flush())
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool { return len(api.syncs()) == 2 }),
		wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
	time.Sleep(100 * time.Millisecond)
	must.Eq(t, []string{"apps", "apps"}, api.syncs(), must.Sprint("one instance per message"))
}

func TestConfig_SyncRegionsForwardsRegistryEvents(t *testing.T) {
	env := func(string) string { return "" }
	cfg, err := ParseConfig([]string{"-sync-regions", "-topics", "Job,CustomObject:db.example.com/Cluster"}, env, nil)
	must.NoError(t, err)
	must.Eq(t, []TopicFilter{
		{Topic: "Job"},
		{Topic: "CustomObject", Keys: []string{"db.example.com/Cluster", RegistryKindRef}},
	}, cfg.Topics)

	cfg, err = ParseConfig([]string{"-sync-regions", "-topics", "Job"}, env, nil)
	must.NoError(t, err)
	must.Eq(t, []TopicFilter{{Topic: "Job"}, registryTopic}, cfg.Topics)

	cfg, err = ParseConfig([]string{"-sync-regions", "-topics", "CustomObject"}, env, nil)
	must.NoError(t, err)
	must.Eq(t, []TopicFilter{{Topic: "CustomObject"}}, cfg.Topics, must.Sprint("all object events already"))

	_, n, err := ParsePushConfig([]string{"-source", "oci://ghcr.io/org/packs", "-pack", "redis"}, env, nil)
	must.NoError(t, err)
	must.Eq(t, "redis", n.Pack)
	_, _, err = ParsePushConfig(nil, env, nil)
	must.ErrorContains(t, err, "-source or -registry")
}
