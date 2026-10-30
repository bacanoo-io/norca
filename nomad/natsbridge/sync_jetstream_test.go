// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

const syncPrefix = "norca.events"

// runSyncers runs n syncers of the region on durable consumers until the
// returned stop function is called.
func runSyncers(t *testing.T, js jetstream.JetStream, api RegistryAPI, region string, n int) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		s := &Syncer{API: api, Region: region, Logger: testLogger(t)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Consume(ctx, js, SyncConsumerOptions{
				Prefix: syncPrefix, Pushes: true, PushStream: "PUSHES", Regions: true, Discover: 50 * time.Millisecond,
			})
		}()
	}
	stop = func() { cancel(); wg.Wait() }
	t.Cleanup(stop)
	return stop
}

// waitConsumer waits until the consumer of the region exists on the stream:
// a new consumer only gets the messages published after its creation.
func waitConsumer(t *testing.T, js jetstream.JetStream, stream, region string) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		_, err := js.Consumer(context.Background(), stream, "norca-sync-"+region)
		return err == nil
	}), wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
}

func waitSyncs(t *testing.T, api *fakeRegistries, n int) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool { return len(api.syncs()) >= n }),
		wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
	time.Sleep(100 * time.Millisecond)
	must.Eq(t, n, len(api.syncs()), must.Sprint("each message is handled once"))
}

// shortSyncDelays shortens the redelivery delays for a test.
func shortSyncDelays(t *testing.T) {
	ackWait, retry := syncAckWait, syncRetryDelay
	syncAckWait, syncRetryDelay = time.Second, 100*time.Millisecond
	t.Cleanup(func() { syncAckWait, syncRetryDelay = ackWait, retry })
}

func TestSyncer_ConsumeDurable(t *testing.T) {
	shortSyncDelays(t)
	srv := runNATS(t, true)
	nc := connect(t, srv)
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	ctx := context.Background()
	set := StreamSettings{Duplicates: 10 * time.Minute, Replicas: 1, Storage: StorageFile}

	res, err := EnsurePushStream(ctx, js, "PUSHES", syncPrefix, set)
	must.NoError(t, err)
	must.True(t, res.Created)
	info := streamConfig(t, js, "PUSHES")
	must.Eq(t, []string{syncPrefix + ".packs.pushed", syncPrefix + ".packs.pushed.>"}, info.Subjects)
	must.Eq(t, 24*time.Hour, info.MaxAge)

	// The event streams of the own region and of another one.
	for _, r := range []string{"eu", "us"} {
		_, err := EnsureStream(ctx, js, "EVENTS_"+r, syncPrefix, r, set)
		must.NoError(t, err)
	}

	api := &fakeRegistries{regs: []*Registry{registry(t, "apps", "oci", "oci://ghcr.io/org/packs", "old")}}
	stop := runSyncers(t, js, api, "eu", 2)
	waitConsumer(t, js, "PUSHES", "eu")
	waitConsumer(t, js, "EVENTS_us", "eu")
	_, err = js.Consumer(ctx, "EVENTS_eu", "norca-sync-eu")
	must.ErrorIs(t, err, jetstream.ErrConsumerNotFound, must.Sprint("no consumer on the own region's stream"))

	// A push notice and a registry event of the other region: one of the
	// two instances handles each.
	must.NoError(t, PublishPush(nc, syncPrefix, &PushNotice{Source: "oci://ghcr.io/org/packs"}))
	waitSyncs(t, api, 1)
	_, err = js.Publish(ctx, syncPrefix+".us.CustomObject",
		registryEventJSON("us", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/org/packs", "new"))
	must.NoError(t, err)
	waitSyncs(t, api, 2)

	// While no instance runs, the messages are kept and handled when one
	// starts again.
	stop()
	must.NoError(t, PublishPush(nc, syncPrefix, &PushNotice{Registry: "apps"}))
	_, err = js.Publish(ctx, syncPrefix+".us.CustomObject",
		registryEventJSON("us", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/org/packs", "newer"))
	must.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	must.Eq(t, 2, len(api.syncs()))
	runSyncers(t, js, api, "eu", 1)
	waitSyncs(t, api, 4)

	// The event stream of a region that appears later is found.
	_, err = EnsureStream(ctx, js, "EVENTS_ap", syncPrefix, "ap", set)
	must.NoError(t, err)
	waitConsumer(t, js, "EVENTS_ap", "eu")
	_, err = js.Publish(ctx, syncPrefix+".ap.CustomObject",
		registryEventJSON("ap", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/org/packs", "newest"))
	must.NoError(t, err)
	waitSyncs(t, api, 5)
}

func TestSyncer_ConsumeRetriesWhenTheAPIFails(t *testing.T) {
	shortSyncDelays(t)
	srv := runNATS(t, true)
	nc := connect(t, srv)
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	ctx := context.Background()
	_, err = EnsurePushStream(ctx, js, "PUSHES", syncPrefix, StreamSettings{Replicas: 1, Storage: StorageFile})
	must.NoError(t, err)

	api := &fakeRegistries{regs: []*Registry{registry(t, "apps", "oci", "oci://ghcr.io/org/packs", "old")}, failLists: 2}
	runSyncers(t, js, api, "eu", 1)
	waitConsumer(t, js, "PUSHES", "eu")

	// An invalid notice is dropped, not redelivered.
	_, err = js.Publish(ctx, syncPrefix+".packs.pushed", []byte("not json"))
	must.NoError(t, err)
	// The API fails twice: the notice is redelivered until it is handled.
	must.NoError(t, PublishPush(nc, syncPrefix, &PushNotice{Registry: "apps"}))
	waitSyncs(t, api, 1)

	cons, err := js.Consumer(ctx, "PUSHES", "norca-sync-eu")
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		info, err := cons.Info(ctx)
		return err == nil && info.NumAckPending == 0 && info.NumPending == 0
	}), wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
}

// bridgeSyncPublish are the publish permissions the nats pack gives the
// bridge's user with the inbound mode on JetStream (templates/_helpers.tpl).
var bridgeSyncPublish = []string{
	syncPrefix + ".>",
	"$JS.API.STREAM.INFO.*", "$JS.API.STREAM.CREATE.*", "$JS.API.STREAM.UPDATE.*",
	"$JS.API.STREAM.NAMES",
	"$JS.API.CONSUMER.CREATE.>", "$JS.API.CONSUMER.DURABLE.CREATE.>",
	"$JS.API.CONSUMER.INFO.>", "$JS.API.CONSUMER.MSG.NEXT.>",
	"$JS.ACK.>",
}

// TestSyncer_ConsumeWithBridgePermissions runs the bridge's streams and
// consumers as a user with only the permissions of the pack.
func TestSyncer_ConsumeWithBridgePermissions(t *testing.T) {
	shortSyncDelays(t)
	opts := &server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir(),
		Users: []*server.User{
			{Username: "admin", Password: "admin"},
			{Username: "bridge", Password: "bridge", Permissions: &server.Permissions{
				Publish:   &server.SubjectPermission{Allow: bridgeSyncPublish},
				Subscribe: &server.SubjectPermission{Allow: []string{"_INBOX.>"}},
			}},
		},
	}
	srv, err := server.NewServer(opts)
	must.NoError(t, err)
	go srv.Start()
	must.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })

	conn := func(user string) *nats.Conn {
		nc, err := nats.Connect(srv.ClientURL(), nats.UserInfo(user, user))
		must.NoError(t, err)
		t.Cleanup(nc.Close)
		return nc
	}
	ctx := context.Background()
	admin := conn("admin")
	adminJS, err := jetstream.New(admin)
	must.NoError(t, err)
	// The event stream of another region, created by that region's bridge.
	_, err = EnsureStream(ctx, adminJS, "EVENTS_us", syncPrefix, "us", StreamSettings{Replicas: 1, Storage: StorageFile})
	must.NoError(t, err)

	// As the bridge: its own event stream, the push stream, the consumers.
	js, err := jetstream.New(conn("bridge"))
	must.NoError(t, err)
	set := StreamSettings{Duplicates: 10 * time.Minute, Replicas: 1, Storage: StorageFile, MaxAge: time.Hour}
	_, err = EnsureStream(ctx, js, "EVENTS_eu", syncPrefix, "eu", set)
	must.NoError(t, err)
	_, err = EnsurePushStream(ctx, js, "PUSHES", syncPrefix, set)
	must.NoError(t, err)
	set.MaxAge = 2 * time.Hour
	res, err := EnsureStream(ctx, js, "EVENTS_eu", syncPrefix, "eu", set)
	must.NoError(t, err)
	must.Eq(t, []string{"max_age"}, res.Updated)

	api := &fakeRegistries{regs: []*Registry{registry(t, "apps", "oci", "oci://ghcr.io/org/packs", "old")}}
	runSyncers(t, js, api, "eu", 1)
	waitConsumer(t, adminJS, "PUSHES", "eu")
	waitConsumer(t, adminJS, "EVENTS_us", "eu")

	must.NoError(t, PublishPush(admin, syncPrefix, &PushNotice{Registry: "apps"}))
	waitSyncs(t, api, 1)
	_, err = adminJS.Publish(ctx, syncPrefix+".us.CustomObject",
		registryEventJSON("us", "CustomObjectStatusUpdated", "apps", "oci", "oci://ghcr.io/org/packs", "new"))
	must.NoError(t, err)
	waitSyncs(t, api, 2)
}
