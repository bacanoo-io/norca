// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"fmt"
	"strconv"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The secrets watcher runs on the leader and drives the lifecycle of the
// secrets of the built-in store (norca-doc content/docs/design/secrets-plan.md,
// "Rotation and dynamic secrets"). For every live secret (current version
// neither deleted nor destroyed) it announces on the Secret topic of the event
// stream, with metadata only:
//
//   - SecretExpiring once expires_at is within the expiry warning window,
//     SecretExpired once it has passed;
//   - SecretRotationDue once rotate_every has elapsed since the current
//     version was written.
//
// Each reminder is published once per secret version, condition and setting
// (a changed expires_at or rotate_every is evaluated anew). The record of
// what was announced is kept in memory only: a new leader announces the
// conditions that hold once more, and never again after that.
//
// A secret with a rotation job (rotate { job = "..." }) has that
// parameterized job dispatched when its rotation is due, once per version,
// with the leader's own authority. The dispatch carries an idempotency token
// derived from the secret and its version, so a new leader that finds the
// child job of a former leader's dispatch does not dispatch again (a token
// protects only while the child job exists: once it is garbage collected, a
// new leader dispatches again if the version is still unrotated). Failed
// dispatches are announced (SecretRotationFailed), logged and retried with
// an exponential backoff. Every dispatch attempt is an audit record of the
// "rotate" operation, with the server component as the accessor.

const (
	// secretExpiryWarningDefault is the default expiry warning window.
	secretExpiryWarningDefault = 7 * 24 * time.Hour

	// secretsWatcherInterval is the longest time between two evaluations;
	// the watcher wakes earlier for a change of the secrets table and for
	// the next condition it knows of.
	secretsWatcherInterval = time.Minute

	// secretsWatcherMinWait bounds the evaluations caused by timers.
	secretsWatcherMinWait = time.Second

	// The backoff of failed rotation dispatches doubles from the minimum up
	// to the maximum.
	secretRotationBackoffMin = time.Minute
	secretRotationBackoffMax = time.Hour

	// secretRotationAccessor is the system accessor of the audit records
	// of rotation dispatches.
	secretRotationAccessor = "secret-rotation"
)

// secretReminderKey identifies an announced reminder: the secret (its
// create index tells a recreated secret apart), its version, the event type
// and the setting it was computed from.
type secretReminderKey struct {
	namespace, path      string
	createIndex, version uint64
	typ                  string
	setting              int64
}

// secretRotationKey identifies the rotation of one version of a secret.
type secretRotationKey struct {
	namespace, path      string
	createIndex, version uint64
}

// secretRotationState is the dispatch state of the rotation of a version.
type secretRotationState struct {
	// done is set once the job was dispatched (or found dispatched).
	done bool

	// job is the rotation job the attempts were made for; a changed job
	// resets the backoff.
	job string

	attempts int
	next     time.Time
}

// secretsWatcherCounts are the numbers of live secrets per condition, the
// gauges of the watcher.
type secretsWatcherCounts struct {
	expiring, expired, rotationDue int
}

// secretsWatcher is the state of the secrets watcher of one leadership term.
// It is used by one goroutine only.
type secretsWatcher struct {
	srv    *Server
	logger hclog.Logger

	warning  time.Duration
	interval time.Duration

	// now and publish are replaced in tests.
	now     func() time.Time
	publish func(events []structs.Event)

	announced map[secretReminderKey]struct{}
	rotations map[secretRotationKey]*secretRotationState

	// counts are the gauges of the last evaluation.
	counts secretsWatcherCounts
}

func newSecretsWatcher(s *Server) *secretsWatcher {
	w := &secretsWatcher{
		srv:       s,
		logger:    s.logger.Named("secrets_watcher"),
		warning:   s.config.SecretsExpiryWarning,
		interval:  secretsWatcherInterval,
		now:       time.Now,
		announced: map[secretReminderKey]struct{}{},
		rotations: map[secretRotationKey]*secretRotationState{},
	}
	if w.warning <= 0 {
		w.warning = secretExpiryWarningDefault
	}
	w.publish = w.publishEvents
	return w
}

// runSecretsWatcher runs the secrets watcher until stopCh is closed, which
// happens when the server loses leadership.
func (s *Server) runSecretsWatcher(stopCh chan struct{}) {
	if s.config.secretsWatcherDisabled {
		return
	}
	newSecretsWatcher(s).run(stopCh)
}

func (w *secretsWatcher) run(stopCh <-chan struct{}) {
	// Followers report no secrets, so that the gauges of a former leader do
	// not add to the current leader's.
	defer w.setGauges(secretsWatcherCounts{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	for {
		ws := memdb.NewWatchSet()
		next := w.evaluate(ws)

		wait := w.interval
		if !next.IsZero() {
			if d := next.Sub(w.now()); d < wait {
				wait = max(d, secretsWatcherMinWait)
			}
		}
		waitCtx, waitCancel := context.WithTimeout(ctx, wait)
		_ = ws.WatchCtx(waitCtx)
		waitCancel()
		if ctx.Err() != nil {
			return
		}
	}
}

// evaluate checks every secret once: it publishes the reminders not
// announced yet, dispatches the due rotation jobs, sets the gauges and
// returns the next time a condition changes or a dispatch is retried (zero
// if none is known). ws watches the secrets table.
func (w *secretsWatcher) evaluate(ws memdb.WatchSet) time.Time {
	store := w.srv.State()
	iter, err := store.Secrets(ws)
	if err != nil {
		w.logger.Error("failed to list secrets", "error", err)
		return w.now().Add(w.interval)
	}

	now := w.now()
	var next time.Time
	soon := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}

	var counts secretsWatcherCounts
	var events []structs.Event
	announced := make(map[secretReminderKey]struct{}, len(w.announced))
	remind := func(sec *structs.Secret, typ string, setting int64, payload *structs.SecretLifecycleEvent) {
		key := secretReminderKey{
			namespace: sec.Namespace, path: sec.Path,
			createIndex: sec.CreateIndex, version: sec.CurrentVersion,
			typ: typ, setting: setting,
		}
		announced[key] = struct{}{}
		if _, ok := w.announced[key]; ok {
			return
		}
		events = append(events, secretLifecycleEvent(typ, sec, payload))
	}

	type dueRotation struct {
		secret *structs.Secret
		dueAt  time.Time
	}
	var due []dueRotation

	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		sec := raw.(*structs.Secret)
		if sec.Deleted || sec.Destroyed || sec.CurrentVersion == 0 {
			continue
		}

		if sec.ExpiresAt != nil {
			exp := *sec.ExpiresAt
			payload := &structs.SecretLifecycleEvent{ExpiresAt: &exp}
			switch {
			case !now.Before(exp):
				counts.expired++
				remind(sec, structs.TypeSecretExpired, exp.UnixNano(), payload)
			case exp.Sub(now) <= w.warning:
				counts.expiring++
				remind(sec, structs.TypeSecretExpiring, exp.UnixNano(), payload)
				soon(exp)
			default:
				soon(exp.Add(-w.warning))
			}
		}

		// The rotation of a replica is the business of the authoritative
		// region, which writes the new version that is then replicated.
		if sec.RotateEvery > 0 && sec.Replica == nil {
			dueAt := secretVersionTime(sec).Add(sec.RotateEvery)
			if now.Before(dueAt) {
				soon(dueAt)
				continue
			}
			counts.rotationDue++
			payload := &structs.SecretLifecycleEvent{RotationDueAt: &dueAt}
			if sec.Rotate != nil {
				payload.Job = sec.Rotate.Job
			}
			remind(sec, structs.TypeSecretRotationDue, int64(sec.RotateEvery), payload)
			if sec.Rotate != nil && sec.Rotate.Job != "" {
				due = append(due, dueRotation{secret: sec, dueAt: dueAt})
			}
		}
	}
	w.announced = announced
	if len(events) > 0 {
		w.publish(events)
	}

	rotations := make(map[secretRotationKey]*secretRotationState, len(due))
	for _, d := range due {
		key := secretRotationKey{
			namespace: d.secret.Namespace, path: d.secret.Path,
			createIndex: d.secret.CreateIndex, version: d.secret.CurrentVersion,
		}
		st := w.rotations[key]
		if st == nil {
			st = &secretRotationState{}
		}
		rotations[key] = st
		if retry := w.rotate(d.secret, d.dueAt, st, now); !retry.IsZero() {
			soon(retry)
		}
	}
	w.rotations = rotations

	w.counts = counts
	w.setGauges(counts)
	return next
}

// rotate dispatches the rotation job of a secret whose rotation is due,
// unless it was dispatched already or a failed attempt's backoff has not
// passed. It returns when to try again, zero if nothing is left to do.
func (w *secretsWatcher) rotate(sec *structs.Secret, dueAt time.Time, st *secretRotationState, now time.Time) time.Time {
	if st.done {
		return time.Time{}
	}
	job := sec.Rotate.Job
	if st.job != job {
		st.job, st.attempts, st.next = job, 0, time.Time{}
	}
	if now.Before(st.next) {
		return st.next
	}
	logger := w.logger.With("namespace", sec.Namespace, "path", sec.Path,
		"version", sec.CurrentVersion, "job", job)

	// A rotation job could not write the new version while the store is
	// unavailable; wait for it without counting an attempt.
	if _, err := w.srv.secretCore(); err != nil {
		logger.Debug("postponing the rotation dispatch, the secret store is unavailable", "error", err)
		return now.Add(w.interval)
	}

	token := secretRotationToken(sec)
	if childID, ok := w.srv.secretRotationDispatched(sec.Namespace, job, token); ok {
		// Dispatched by a former leader.
		logger.Debug("rotation job already dispatched", "dispatched_job_id", childID)
		st.done = true
		return time.Time{}
	}

	rec := &structs.SecretAccessEvent{
		Region:    w.srv.Region(),
		Server:    w.srv.config.NodeName,
		Namespace: sec.Namespace,
		Path:      sec.Path,
		Version:   sec.CurrentVersion,
		Operation: structs.SecretOpRotate,
		Accessor:  &structs.SecretAccessor{System: secretRotationAccessor},
		Job:       job,
	}
	var reply *structs.JobDispatchResponse
	err := w.srv.secretAudit(rec, false)
	if err == nil {
		reply, err = w.srv.secretRotationDispatch(sec, job, token)
	}
	rec.Result = secretAccessResult(err)
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.DispatchedJobID = reply.DispatchedJobID
	}
	_ = w.srv.secretAudit(rec, true)

	payload := &structs.SecretLifecycleEvent{RotationDueAt: &dueAt, Job: job}
	if err == nil {
		st.done = true
		metrics.IncrCounter([]string{"nomad", "secrets", "rotation", "dispatched"}, 1)
		logger.Info("dispatched the rotation job", "dispatched_job_id", reply.DispatchedJobID)
		payload.DispatchedJobID, payload.EvalID = reply.DispatchedJobID, reply.EvalID
		w.publish([]structs.Event{secretLifecycleEvent(structs.TypeSecretRotationDispatched, sec, payload)})
		return time.Time{}
	}

	st.attempts++
	backoff := secretRotationBackoffMax
	if st.attempts <= 7 {
		backoff = min(secretRotationBackoffMin<<(st.attempts-1), secretRotationBackoffMax)
	}
	st.next = now.Add(backoff)
	metrics.IncrCounter([]string{"nomad", "secrets", "rotation", "failed"}, 1)
	logger.Warn("failed to dispatch the rotation job", "attempt", st.attempts,
		"retry_in", backoff, "error", err)
	next := st.next
	payload.Error, payload.Attempt, payload.NextAttempt = err.Error(), st.attempts, &next
	w.publish([]structs.Event{secretLifecycleEvent(structs.TypeSecretRotationFailed, sec, payload)})
	return st.next
}

// setGauges publishes the numbers of secrets per condition.
func (w *secretsWatcher) setGauges(c secretsWatcherCounts) {
	metrics.SetGauge([]string{"nomad", "secrets", "expiring"}, float32(c.expiring))
	metrics.SetGauge([]string{"nomad", "secrets", "expired"}, float32(c.expired))
	metrics.SetGauge([]string{"nomad", "secrets", "rotation_due"}, float32(c.rotationDue))
}

// publishEvents publishes lifecycle events on this server's event broker.
// They are not written to Raft: only the leader produces them.
func (w *secretsWatcher) publishEvents(events []structs.Event) {
	store := w.srv.State()
	broker, err := store.EventBroker()
	if err != nil {
		// The event broker is disabled.
		return
	}
	index, _ := store.LatestIndex()
	for i := range events {
		events[i].Index = index
	}
	broker.Publish(&structs.Events{Index: index, Events: events})
}

// secretLifecycleEvent builds a lifecycle event of the Secret topic; payload
// is completed with the secret's metadata.
func secretLifecycleEvent(typ string, sec *structs.Secret, payload *structs.SecretLifecycleEvent) structs.Event {
	payload.Secret = sec.Stub()
	payload.Version = sec.CurrentVersion
	return structs.Event{
		Topic:     structs.TopicSecret,
		Type:      typ,
		Key:       sec.Path,
		Namespace: sec.Namespace,
		Payload:   payload,
	}
}

// secretVersionTime is when the current version of a secret was written.
func secretVersionTime(sec *structs.Secret) time.Time {
	if v := sec.Version(sec.CurrentVersion); v != nil && v.CreateTime > 0 {
		return time.Unix(0, v.CreateTime)
	}
	return time.Unix(0, sec.ModifyTime)
}

// secretRotationToken is the dispatch idempotency token of the rotation of
// the current version of a secret.
func secretRotationToken(sec *structs.Secret) string {
	return fmt.Sprintf("norca-secret-rotation:%d:%d:%s", sec.CreateIndex, sec.CurrentVersion, sec.Path)
}

// secretRotationDispatched looks for the child job of a rotation dispatch
// with the idempotency token and returns its ID.
func (s *Server) secretRotationDispatched(namespace, jobID, token string) (string, bool) {
	iter, err := s.State().JobsByIDPrefix(nil, namespace, jobID)
	if err != nil {
		return "", false
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		job := raw.(*structs.Job)
		if job.ParentID == jobID && job.DispatchIdempotencyToken == token {
			return job.ID, true
		}
	}
	return "", false
}

// secretRotationDispatch dispatches the rotation job of a secret through
// Job.Dispatch with the leader's token. The dispatch meta names the secret:
// the job must declare structs.SecretRotateMetaPath, the namespace and the
// version are passed if it declares them.
func (s *Server) secretRotationDispatch(sec *structs.Secret, jobID, token string) (*structs.JobDispatchResponse, error) {
	job, err := s.State().JobByID(nil, sec.Namespace, jobID)
	switch {
	case err != nil:
		return nil, err
	case job == nil:
		return nil, fmt.Errorf("rotation job %q not found in namespace %q", jobID, sec.Namespace)
	case !job.IsParameterized():
		return nil, fmt.Errorf("rotation job %q is not a parameterized job", jobID)
	}
	declared := map[string]bool{}
	for _, k := range job.ParameterizedJob.MetaRequired {
		declared[k] = true
	}
	for _, k := range job.ParameterizedJob.MetaOptional {
		declared[k] = true
	}
	if !declared[structs.SecretRotateMetaPath] {
		return nil, fmt.Errorf("rotation job %q must declare the dispatch meta key %q (meta_required or meta_optional)",
			jobID, structs.SecretRotateMetaPath)
	}
	meta := map[string]string{structs.SecretRotateMetaPath: sec.Path}
	if declared[structs.SecretRotateMetaNamespace] {
		meta[structs.SecretRotateMetaNamespace] = sec.Namespace
	}
	if declared[structs.SecretRotateMetaVersion] {
		meta[structs.SecretRotateMetaVersion] = strconv.FormatUint(sec.CurrentVersion, 10)
	}

	args := &structs.JobDispatchRequest{
		JobID: jobID,
		Meta:  meta,
		WriteRequest: structs.WriteRequest{
			Region:           s.Region(),
			Namespace:        sec.Namespace,
			AuthToken:        s.getLeaderAcl(),
			IdempotencyToken: token,
		},
	}
	var reply structs.JobDispatchResponse
	if err := s.RPC("Job.Dispatch", args, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
