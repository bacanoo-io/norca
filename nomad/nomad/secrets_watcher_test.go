// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/nomad/mock"
	"github.com/bacanoo-io/norca/nomad/nomad/stream"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"
)

// watcherTestServer starts a server whose leader runs no secrets watcher, so
// that the tests drive their own watchers with a controlled clock.
func watcherTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	s, _, cleanup := secretTestServer(t, false, func(c *Config) {
		c.secretsWatcherDisabled = true
	})
	return s, cleanup
}

// testWatcher is a secrets watcher with a settable clock that records the
// events it publishes.
type testWatcher struct {
	*secretsWatcher
	clock  time.Time
	events []structs.Event
}

func newTestWatcher(s *Server, clock time.Time) *testWatcher {
	tw := &testWatcher{secretsWatcher: newSecretsWatcher(s), clock: clock}
	tw.now = func() time.Time { return tw.clock }
	tw.publish = func(events []structs.Event) { tw.events = append(tw.events, events...) }
	return tw
}

// run evaluates once and returns the events published by the evaluation
// as "<type> <path>" and the next wake up time.
func (tw *testWatcher) run() ([]string, time.Time) {
	tw.events = nil
	next := tw.evaluate(memdb.NewWatchSet())
	out := []string{}
	for _, e := range tw.events {
		out = append(out, e.Type+" "+e.Key)
	}
	return out, next
}

func writeSecretMeta(t *testing.T, s *Server, path string, items structs.SecretItems, meta *structs.SecretMeta) *structs.Secret {
	t.Helper()
	req := secretWriteReq(path, "", items)
	req.Meta = meta
	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, req, &w))
	return w.Secret
}

func TestSecretsWatcher_Reminders(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := watcherTestServer(t)
	defer cleanup()

	start := time.Now().UTC().Truncate(time.Second)
	at := func(d time.Duration) *time.Time { t := start.Add(d); return &t }
	items := structs.SecretItems{"password": "x"}

	writeSecretMeta(t, s, "exp/soon", items, &structs.SecretMeta{ExpiresAt: at(48 * time.Hour)})
	writeSecretMeta(t, s, "exp/past", items, &structs.SecretMeta{ExpiresAt: at(-time.Hour)})
	writeSecretMeta(t, s, "exp/far", items, &structs.SecretMeta{ExpiresAt: at(30 * 24 * time.Hour)})
	writeSecretMeta(t, s, "rot/due", items, &structs.SecretMeta{RotateEvery: time.Hour})
	writeSecretMeta(t, s, "gone", items, &structs.SecretMeta{ExpiresAt: at(-time.Hour)})
	var del structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretDeleteRPCMethod, secretVersionsReq("gone", ""), &del))

	// Two hours later: one secret in each condition; the deleted one is
	// not live.
	w := newTestWatcher(s, start.Add(2*time.Hour))
	events, next := w.run()
	must.SliceContainsAll(t, []string{
		"SecretExpired exp/past", "SecretExpiring exp/soon", "SecretRotationDue rot/due",
	}, events)
	must.Eq(t, secretsWatcherCounts{expiring: 1, expired: 1, rotationDue: 1}, w.counts)
	must.True(t, at(48*time.Hour).Equal(next), must.Sprint(next))
	for _, e := range w.events {
		p := e.Payload.(*structs.SecretLifecycleEvent)
		must.Eq(t, 1, p.Version)
		must.Eq(t, e.Key, p.Secret.Path)
		must.Eq(t, structs.TopicSecret, e.Topic)
	}

	// Announced once: the next evaluations publish nothing.
	events, _ = w.run()
	must.SliceEmpty(t, events)
	events, _ = w.run()
	must.SliceEmpty(t, events)

	// A changed expiry is evaluated anew, without a new version.
	writeSecretMeta(t, s, "exp/soon", nil, &structs.SecretMeta{ExpiresAt: at(72 * time.Hour)})
	events, _ = w.run()
	must.Eq(t, []string{"SecretExpiring exp/soon"}, events)

	// A new version is not due yet; it is announced once its interval has
	// elapsed.
	written := writeSecretMeta(t, s, "rot/due", items, nil)
	must.Eq(t, 2, written.CurrentVersion)
	w.clock = time.Now()
	events, _ = w.run()
	must.SliceEmpty(t, events)
	must.Eq(t, 0, w.counts.rotationDue)
	w.clock = time.Now().Add(2 * time.Hour)
	events, _ = w.run()
	must.Eq(t, []string{"SecretRotationDue rot/due"}, events)
	must.Eq(t, 2, w.events[0].Payload.(*structs.SecretLifecycleEvent).Version)

	// The far expiry enters the warning window.
	w.clock = start.Add(24 * 24 * time.Hour)
	events, _ = w.run()
	must.SliceContains(t, events, "SecretExpiring exp/far")

	// A new leader announces what holds once more, then nothing.
	w2 := newTestWatcher(s, w.clock)
	events, _ = w2.run()
	must.SliceNotEmpty(t, events)
	events, _ = w2.run()
	must.SliceEmpty(t, events)

	// A configured warning window applies.
	w3 := newTestWatcher(s, start.Add(2*time.Hour))
	w3.warning = time.Hour
	w3.run()
	must.Eq(t, 0, w3.counts.expiring)
}

// registerRotationJob registers a parameterized batch job directly in the
// state store.
func registerRotationJob(t *testing.T, s *Server, id string, params *structs.ParameterizedJobConfig) *structs.Job {
	t.Helper()
	job := mock.BatchJob()
	job.ID = id
	job.Name = id
	job.ParameterizedJob = params
	index, err := s.State().LatestIndex()
	must.NoError(t, err)
	must.NoError(t, s.State().UpsertJob(structs.MsgTypeTestSetup, index+1, nil, job))
	return job
}

// rotationChildren returns the dispatched children of a rotation job.
func rotationChildren(t *testing.T, s *Server, parent string) []*structs.Job {
	t.Helper()
	iter, err := s.State().JobsByIDPrefix(nil, structs.DefaultNamespace, parent)
	must.NoError(t, err)
	var out []*structs.Job
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		if job := raw.(*structs.Job); job.ParentID == parent {
			out = append(out, job)
		}
	}
	return out
}

func TestSecretsWatcher_RotationDispatch(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := watcherTestServer(t)
	defer cleanup()

	registerRotationJob(t, s, "rotate-db", &structs.ParameterizedJobConfig{
		MetaRequired: []string{structs.SecretRotateMetaPath},
		MetaOptional: []string{structs.SecretRotateMetaNamespace, structs.SecretRotateMetaVersion},
	})
	secret := writeSecretMeta(t, s, "db/pw", structs.SecretItems{"password": secretCanary},
		&structs.SecretMeta{RotateEvery: time.Hour, Rotate: &structs.SecretRotate{Job: "rotate-db"}})

	broker, err := s.State().EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecret: {"*"}, structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	// Not due yet: nothing is dispatched.
	w := newSecretsWatcher(s)
	w.evaluate(memdb.NewWatchSet())
	must.SliceEmpty(t, rotationChildren(t, s, "rotate-db"))

	// Due: the job is dispatched with the secret's path, namespace and
	// version, through the real event broker.
	later := time.Now().Add(2 * time.Hour)
	w.now = func() time.Time { return later }
	w.evaluate(memdb.NewWatchSet())
	children := rotationChildren(t, s, "rotate-db")
	must.Len(t, 1, children)
	child := children[0]
	must.Eq(t, map[string]string{
		structs.SecretRotateMetaPath:      "db/pw",
		structs.SecretRotateMetaNamespace: structs.DefaultNamespace,
		structs.SecretRotateMetaVersion:   "1",
	}, child.Meta)
	must.Eq(t, secretRotationToken(secret), child.DispatchIdempotencyToken)

	// The dispatch is announced and audited, with the system accessor and
	// without values.
	var dispatched *structs.SecretLifecycleEvent
	var audit *structs.SecretAccessEvent
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for dispatched == nil || audit == nil {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			switch p := e.Payload.(type) {
			case *structs.SecretLifecycleEvent:
				if e.Type == structs.TypeSecretRotationDispatched {
					dispatched = p
				}
			case *structs.SecretAccessEvent:
				if p.Operation == structs.SecretOpRotate {
					audit = p
				}
			}
		}
	}
	must.Eq(t, child.ID, dispatched.DispatchedJobID)
	must.Eq(t, "rotate-db", dispatched.Job)
	must.Eq(t, structs.SecretAccessResultOK, audit.Result)
	must.Eq(t, secretRotationAccessor, audit.Accessor.System)
	must.Eq(t, child.ID, audit.DispatchedJobID)
	must.Eq(t, 1, audit.Version)
	must.Eq(t, 1, w.counts.rotationDue)

	// Once per version: later evaluations do not dispatch again.
	w.evaluate(memdb.NewWatchSet())
	must.Len(t, 1, rotationChildren(t, s, "rotate-db"))

	// A new leader finds the child job by its idempotency token: it
	// announces the due rotation again but neither dispatches nor
	// audits a second time.
	w2 := newTestWatcher(s, later)
	events, _ := w2.run()
	must.Eq(t, []string{"SecretRotationDue db/pw"}, events)
	must.Len(t, 1, rotationChildren(t, s, "rotate-db"))

	// The job writes the new version: the rotation of version 2 is due
	// one interval later and dispatched again.
	writeSecretMeta(t, s, "db/pw", structs.SecretItems{"password": "rotated"}, nil)
	w2.clock = time.Now()
	events, _ = w2.run()
	must.SliceEmpty(t, events)
	w2.clock = time.Now().Add(2 * time.Hour)
	events, _ = w2.run()
	must.Eq(t, []string{"SecretRotationDue db/pw", "SecretRotationDispatched db/pw"}, events)
	children = rotationChildren(t, s, "rotate-db")
	must.Len(t, 2, children)
}

func TestSecretsWatcher_RotationFailureBackoff(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := watcherTestServer(t)
	defer cleanup()

	writeSecretMeta(t, s, "db/pw", structs.SecretItems{"password": "x"},
		&structs.SecretMeta{RotateEvery: time.Minute, Rotate: &structs.SecretRotate{Job: "rotate-db"}})

	clock := time.Now().Add(time.Hour)
	w := newTestWatcher(s, clock)
	failure := func() *structs.SecretLifecycleEvent {
		t.Helper()
		for _, e := range w.events {
			if e.Type == structs.TypeSecretRotationFailed {
				return e.Payload.(*structs.SecretLifecycleEvent)
			}
		}
		return nil
	}

	// The job does not exist: reported, retried after a minute.
	_, next := w.run()
	f := failure()
	must.NotNil(t, f)
	must.StrContains(t, f.Error, "not found")
	must.Eq(t, 1, f.Attempt)
	must.True(t, clock.Add(secretRotationBackoffMin).Equal(next), must.Sprint(next))

	// No tight loop: before the backoff has passed, nothing is attempted.
	w.clock = clock.Add(30 * time.Second)
	events, _ := w.run()
	must.SliceEmpty(t, events)

	// The next attempt doubles the backoff.
	w.clock = clock.Add(secretRotationBackoffMin)
	_, next = w.run()
	must.Eq(t, 2, failure().Attempt)
	must.True(t, w.clock.Add(2*secretRotationBackoffMin).Equal(next), must.Sprint(next))

	// Not parameterized, then without the secret_path meta key, then with
	// a required meta key the watcher cannot provide.
	step := func(params *structs.ParameterizedJobConfig, want string) {
		t.Helper()
		registerRotationJob(t, s, "rotate-db", params)
		w.clock = w.clock.Add(secretRotationBackoffMax)
		w.run()
		f := failure()
		must.NotNil(t, f, must.Sprint(want))
		must.StrContains(t, f.Error, want)
	}
	step(nil, "not a parameterized job")
	step(&structs.ParameterizedJobConfig{}, `declare the dispatch meta key "secret_path"`)
	step(&structs.ParameterizedJobConfig{MetaRequired: []string{"secret_path", "other"}}, "required meta keys")
	must.SliceEmpty(t, rotationChildren(t, s, "rotate-db"))

	// Fixed: dispatched after the backoff, with the path only.
	registerRotationJob(t, s, "rotate-db", &structs.ParameterizedJobConfig{MetaOptional: []string{"secret_path"}})
	w.clock = w.clock.Add(secretRotationBackoffMax)
	events, _ = w.run()
	must.Eq(t, []string{"SecretRotationDispatched db/pw"}, events)
	children := rotationChildren(t, s, "rotate-db")
	must.Len(t, 1, children)
	must.Eq(t, map[string]string{"secret_path": "db/pw"}, children[0].Meta)

	// A changed rotation job resets the backoff of a failing rotation.
	writeSecretMeta(t, s, "other", structs.SecretItems{"k": "v"},
		&structs.SecretMeta{RotateEvery: time.Minute, Rotate: &structs.SecretRotate{Job: "missing"}})
	w.clock = time.Now().Add(time.Hour)
	w.run()
	must.NotNil(t, failure())
	writeSecretMeta(t, s, "other", nil,
		&structs.SecretMeta{RotateEvery: time.Minute, Rotate: &structs.SecretRotate{Job: "rotate-db"}})
	events, _ = w.run()
	must.Eq(t, []string{"SecretRotationDispatched other"}, events)
}

// TestSecretsWatcher_Leader checks that the leader runs the watcher and that
// it reacts to a change of the secrets table.
func TestSecretsWatcher_Leader(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, func(c *Config) {
		c.SecretsExpiryWarning = 48 * time.Hour
	})
	defer cleanup()

	broker, err := s.State().EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecret: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	exp := time.Now().Add(24 * time.Hour)
	writeSecretMeta(t, s, "app/cert", structs.SecretItems{"cert": "x"}, &structs.SecretMeta{ExpiresAt: &exp})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			if e.Type == structs.TypeSecretExpiring {
				must.Eq(t, "app/cert", e.Key)
				return
			}
		}
	}
}

// TestSecretEndpoint_RotateJobACL checks that naming a rotation job requires
// dispatch-job, as the leader dispatches it with its own authority.
func TestSecretEndpoint_RotateJobACL(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := secretTestServer(t, true, func(c *Config) {
		c.secretsWatcherDisabled = true
	})
	defer cleanup()
	store := s.fsm.State()
	writer := mock.CreatePolicyAndToken(t, store, 1001, "writer",
		`namespace "default" { capabilities = ["list-secrets", "write-secret"] }`)
	dispatcher := mock.CreatePolicyAndToken(t, store, 1002, "dispatcher",
		`namespace "default" { capabilities = ["list-secrets", "write-secret", "dispatch-job"] }`)

	write := func(token, job string) error {
		req := secretWriteReq("db/pw", token, structs.SecretItems{"password": "x"})
		req.Meta = &structs.SecretMeta{RotateEvery: time.Hour}
		if job != "" {
			req.Meta.Rotate = &structs.SecretRotate{Job: job}
		}
		var w structs.SecretWriteResponse
		return s.RPC(structs.SecretUpsertRPCMethod, req, &w)
	}
	must.NoError(t, write(writer.SecretID, ""))
	must.EqError(t, write(writer.SecretID, "rotate-db"), structs.ErrPermissionDenied.Error())
	must.NoError(t, write(dispatcher.SecretID, "rotate-db"))
	// Keeping the job needs no dispatch-job; changing it does.
	must.NoError(t, write(writer.SecretID, "rotate-db"))
	must.EqError(t, write(writer.SecretID, "other"), structs.ErrPermissionDenied.Error())
	must.NoError(t, write(root.SecretID, "other"))
}
