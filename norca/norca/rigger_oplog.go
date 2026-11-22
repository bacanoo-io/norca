// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The logs of the operations of Rigger (design/rig-integration.md 4.6b): a
// JetStream stream of the bus, a subject per operation, every record of the
// engine a message at every level. The log is followed while the operation
// runs and read in full afterwards, without a load on Raft or the object
// events; it is bounded in size and age (bus.rigger_logs_max_bytes,
// bus.rigger_logs_max_age) and purged when its operation is deleted.

const (
	// riggerLogBuffer is how many records of an operation wait to be
	// published; beyond it records are dropped and counted, so that a slow
	// bus never holds up the engine.
	riggerLogBuffer = 4096

	// riggerLogMaxMsg bounds a record on the bus.
	riggerLogMaxMsg = 64 << 10

	// riggerLogPublishTimeout bounds the publishing of one record.
	riggerLogPublishTimeout = 10 * time.Second

	// riggerLogReadyWait is how long the log of an operation waits for the
	// JetStream of the bus before its records are dropped.
	riggerLogReadyWait = 10 * time.Second
)

// riggerLogSubjects are the subjects of the logs of the operations of the
// region; riggerLogSubject is the subject of one.
func (s *Server) riggerLogSubjects() string { return bus.RiggerLogSubjects(s.config.Region) }

func (s *Server) riggerLogSubject(opID string) string {
	return bus.RiggerLogSubject(s.config.Region, opID)
}

// keepRiggerLogStream creates the stream of the logs of the operations and
// keeps its settings, e.g. the replicas. It runs on the leader.
func (s *Server) keepRiggerLogStream(ctx context.Context, js jetstream.JetStream) error {
	check := time.NewTicker(busStreamCheckInterval)
	defer check.Stop()

	for {
		if err := s.ensureRiggerLogStream(ctx, js); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-check.C:
		}
	}
}

func (s *Server) ensureRiggerLogStream(ctx context.Context, js jetstream.JetStream) error {
	return bus.EnsureStream(ctx, js, bus.RiggerLogStreamName(s.config.Region),
		"Norca Rigger operation logs of region "+s.config.Region,
		[]string{s.riggerLogSubjects()},
		bus.StreamSettings{
			Replicas:   s.busReplicas(),
			MaxAge:     s.config.Bus.RiggerLogsAge(),
			MaxBytes:   s.config.Bus.RiggerLogsBytes(),
			MaxMsgSize: riggerLogMaxMsg,
			Storage:    jetstream.FileStorage,
		})
}

// riggerLogWriter publishes the records of one operation to its subject, in
// order, from a goroutine of its own.
type riggerLogWriter struct {
	srv     *Server
	subject string
	lines   chan []byte
	done    chan struct{}

	// stored is set once a record reached the stream; dropped counts the
	// records that did not.
	stored  atomic.Bool
	dropped atomic.Int64
}

// newRiggerLogWriter starts the writer of the log of an operation. Without
// a bus the records are only dropped (the hclog log of the server has
// them).
func (s *Server) newRiggerLogWriter(subject string) *riggerLogWriter {
	w := &riggerLogWriter{srv: s, subject: subject, lines: make(chan []byte, riggerLogBuffer), done: make(chan struct{})}
	s.riggerOps.openLog(subject)

	go w.run()

	return w
}

func (w *riggerLogWriter) run() {
	defer close(w.done)

	var js jetstream.JetStream

	// Right after a start the JetStream of the bus may not be ready: the
	// records wait in the queue meanwhile.
	if w.srv.bus != nil {
		deadline := time.Now().Add(riggerLogReadyWait)
		for !w.srv.bus.JetStreamReady() && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	}

	if w.srv.bus != nil && w.srv.bus.JetStreamReady() {
		if nc, err := w.srv.bus.Connect(); err == nil {
			defer nc.Close()

			js, _ = jetstream.New(nc)
		}
	}

	// The leader keeps the stream; an operation that starts before the
	// leader made it makes it itself, as the leader would.
	if js != nil {
		ctx, cancel := context.WithTimeout(context.Background(), riggerLogPublishTimeout)
		if err := w.srv.ensureRiggerLogStream(ctx, js); err != nil {
			w.srv.logger.Named("rigger").Warn("the stream of the operation logs could not be made", "error", err)
		}
		cancel()
	}

	for line := range w.lines {
		if js == nil {
			w.dropped.Add(1)

			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), riggerLogPublishTimeout)
		_, err := js.Publish(ctx, w.subject, line)
		cancel()

		if err != nil {
			w.dropped.Add(1)

			continue
		}

		w.stored.Store(true)
	}
}

// send queues a record; a full queue drops it.
func (w *riggerLogWriter) send(line []byte) {
	select {
	case w.lines <- line:
	default:
		w.dropped.Add(1)
	}
}

// close publishes what is queued and ends the writer; it says how many
// records were dropped.
func (w *riggerLogWriter) close() int64 {
	if n := w.dropped.Load(); n > 0 {
		data, _ := json.Marshal(map[string]any{
			"time": time.Now().UTC().Format(time.RFC3339Nano), "level": "WARN",
			"msg": "records of this log were dropped", "dropped": n,
		})
		w.send(data)
	}

	close(w.lines)
	<-w.done
	w.srv.riggerOps.closeLog(w.subject)

	return w.dropped.Load()
}

// busLogHandler is a slog handler that writes every record, at every level,
// as a JSON line to the log of an operation.
type busLogHandler struct {
	w      *riggerLogWriter
	attrs  []slog.Attr
	groups []string
}

func (h *busLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *busLogHandler) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"time": r.Time.UTC().Format(time.RFC3339Nano), "level": r.Level.String(), "msg": r.Message}

	for _, a := range h.attrs {
		addLogAttr(m, a)
	}

	prefix := ""
	for _, g := range h.groups {
		prefix += g + "."
	}

	r.Attrs(func(a slog.Attr) bool {
		a.Key = prefix + a.Key
		addLogAttr(m, a)

		return true
	})

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	if len(data) > riggerLogMaxMsg {
		data, _ = json.Marshal(map[string]any{"time": m["time"], "level": m["level"], "msg": r.Message, "truncated": true})
	}

	h.w.send(data)

	return nil
}

func (h *busLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *h

	prefix := ""
	for _, g := range h.groups {
		prefix += g + "."
	}

	for _, a := range attrs {
		a.Key = prefix + a.Key
		out.attrs = append(append([]slog.Attr(nil), out.attrs...), a)
	}

	return &out
}

func (h *busLogHandler) WithGroup(name string) slog.Handler {
	out := *h
	out.groups = append(append([]string(nil), h.groups...), name)

	return &out
}

// addLogAttr adds an attribute to a record of the log; a group becomes keys
// with its name before them.
func addLogAttr(m map[string]any, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		for _, inner := range v.Group() {
			inner.Key = a.Key + "." + inner.Key
			addLogAttr(m, inner)
		}

		return
	}

	// A duration and a time read as text, as a person reads them, not as
	// the nanoseconds JSON would make of them.
	switch v.Kind() {
	case slog.KindDuration:
		m[a.Key] = v.Duration().String()
	case slog.KindTime:
		m[a.Key] = v.Time().UTC().Format(time.RFC3339Nano)
	default:
		m[a.Key] = v.Any()
	}
}

// teeHandler writes a record to two handlers.
type teeHandler struct{ a, b slog.Handler }

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.a.Enabled(ctx, l) || t.b.Enabled(ctx, l)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error

	if t.a.Enabled(ctx, r.Level) {
		errs = append(errs, t.a.Handle(ctx, r.Clone()))
	}

	if t.b.Enabled(ctx, r.Level) {
		errs = append(errs, t.b.Handle(ctx, r.Clone()))
	}

	return errors.Join(errs...)
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{a: t.a.WithAttrs(attrs), b: t.b.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{a: t.a.WithGroup(name), b: t.b.WithGroup(name)}
}

// RiggerLogLine is a record of the log of an operation and its sequence in
// the stream.
type RiggerLogLine struct {
	Seq  uint64
	Line json.RawMessage
}

// errRiggerLogNoBus is the error of a read of a log on a server without a
// ready bus.
var errRiggerLogNoBus = errors.New("the bus of this server is not ready")

// readRiggerLog reads the records of a subject after the sequence after, at
// most limit, waiting up to wait for the first one.
func (s *Server) readRiggerLog(ctx context.Context, subject string, after uint64, limit int, wait time.Duration) ([]RiggerLogLine, error) {
	if s.bus == nil || !s.bus.JetStreamReady() {
		return nil, errRiggerLogNoBus
	}

	nc, err := s.bus.Connect()
	if err != nil {
		return nil, err
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}

	cfg := jetstream.OrderedConsumerConfig{FilterSubjects: []string{subject}, DeliverPolicy: jetstream.DeliverAllPolicy}
	if after > 0 {
		cfg.DeliverPolicy, cfg.OptStartSeq = jetstream.DeliverByStartSequencePolicy, after+1
	}

	cons, err := js.OrderedConsumer(ctx, bus.RiggerLogStreamName(s.config.Region), cfg)
	if err != nil {
		return nil, err
	}

	var batch jetstream.MessageBatch
	if wait > 0 {
		batch, err = cons.Fetch(limit, jetstream.FetchMaxWait(wait))
	} else {
		batch, err = cons.FetchNoWait(limit)
	}

	if err != nil {
		return nil, err
	}

	var out []RiggerLogLine

	for msg := range batch.Messages() {
		meta, err := msg.Metadata()
		if err != nil {
			continue
		}

		out = append(out, RiggerLogLine{Seq: meta.Sequence.Stream, Line: json.RawMessage(msg.Data())})
	}

	if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
		return out, err
	}

	return out, nil
}

// purgeRiggerLogs removes from the stream the logs that no operation has:
// of an operation deleted on whatever way, and of a start that did not
// become an operation. The logs this server writes stay. The order keeps
// those of the new ones: the subjects are listed first, then the writers
// here and then the state, read by a server that is still the leader; a
// log is written before its operation is, and has its writer until then.
func (s *Server) purgeRiggerLogs(ctx context.Context) error {
	if s.bus == nil || !s.bus.JetStreamReady() {
		return nil
	}

	nc, err := s.bus.Connect()
	if err != nil {
		return err
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}

	stream, err := js.Stream(ctx, bus.RiggerLogStreamName(s.config.Region))
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil
	}

	if err != nil {
		return err
	}

	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(s.riggerLogSubjects()))
	if err != nil {
		return err
	}

	stored := info.State.Subjects
	if len(stored) == 0 {
		return nil
	}

	for _, subject := range s.riggerOps.openLogs() {
		delete(stored, subject)
	}

	if err := s.raft.VerifyLeader().Error(); err != nil {
		return err
	}

	iter, err := s.State().CustomObjects(nil, structs.RiggerObjectGroup, structs.RiggerOperationKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return err
	}

	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		// As stored: an operation that can't be read keeps its log too.
		if subject, ok := raw.(*structs.CustomObject).Status["logSubject"].(string); ok {
			delete(stored, subject)
		}
	}

	for subject := range stored {
		if err := stream.Purge(ctx, jetstream.WithPurgeSubject(subject)); err != nil {
			return err
		}
	}

	return nil
}
