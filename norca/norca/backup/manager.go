// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/version"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	metrics "github.com/hashicorp/go-metrics"
)

// Snapshot is a Raft snapshot archive taken for a backup.
type Snapshot interface {
	Index() uint64
	Term() uint64

	// NewReader returns a reader of the whole archive. It may be called
	// once per destination.
	NewReader() (io.Reader, error)
	Close() error
}

// Deps are the server functions the manager uses.
type Deps struct {
	Logger hclog.Logger

	// Region of the server, and the authoritative region of the federation.
	Region              string
	AuthoritativeRegion string

	// ServerName is the name of this server, recorded as the server that
	// took a backup (TakenBy).
	ServerName string

	// Delegate, if set, asks an observer to take a backup (backup
	// prefer_observer) and returns its record, which the leader then writes
	// to Raft. An error wrapping ErrDelegateRefused means that no observer
	// started anything: the leader takes the backup itself with the same
	// ID. Any other error leaves it open whether an observer still uploads.
	Delegate func(ctx context.Context, req *DelegateRequest) (*structs.BackupRecord, error)

	// TakenByObserver, if set, is called when the leader finishes a backup
	// that an observer took.
	TakenByObserver func()

	// ServersReady reports whether every server runs a Norca version that
	// knows the backup Raft messages.
	ServersReady func() bool

	// Snapshot takes a Raft snapshot (helper/snapshot.New).
	Snapshot func() (Snapshot, error)

	// Keyring returns the root keys of the local keyring.
	Keyring func() ([]*structs.RootKey, error)

	// SecretStoreKey, if set, returns the key of the secret store core
	// (Norca), or nil if the store is not initialized. It is written next to
	// the keyring, so a restore can re-wrap it with the seals of the region.
	SecretStoreKey func() ([]byte, error)

	// KeyringReady, if set, reports whether the keyring is initialized;
	// scheduled backups that include the keyring wait for it.
	KeyringReady func() bool

	// ConsulKV, if set, exports the key-value bucket of the Consul KV of
	// the system bus (Norca), consulkv.Encode; nil without a bus or bucket.
	ConsulKV func(ctx context.Context) ([]byte, error)

	// State returns the current state store.
	State func() *state.StateStore

	// Apply writes a Raft message and returns its index.
	Apply func(msgType structs.MessageType, req any) (uint64, error)
}

// SaveOptions are the options of a single backup.
type SaveOptions struct {
	Trigger string
	Labels  map[string]string
	Pin     bool

	// Started, if set, is called with the backup ID once the snapshot is
	// taken.
	Started func(id string)

	// ID, if set, is the ID of the backup (given by the leader to a backup
	// an observer takes); otherwise it is made from the snapshot index.
	ID string
}

// Manager takes the scheduled and manual backups of a region on the leader,
// applies the retention, and reads backups back for browsing and restore.
type Manager struct {
	logger hclog.Logger
	deps   *Deps

	mu       sync.Mutex
	cfg      *Config
	dests    []Destination
	destsErr error
	leader   bool
	ctx      context.Context
	cancel   context.CancelFunc
	wakeCh   chan struct{}

	// runLock serializes backups and retention runs.
	runLock sync.Mutex

	// failures counts consecutive failures per destination.
	failures map[string]int

	// cache holds the most recently loaded backup.
	cache loadCache

	// restoreLock serializes full restores and their approvals.
	restoreLock sync.Mutex
}

// TryLockRestore takes the lock of full restores, without waiting. It
// returns false if a full restore request is being created or run.
func (m *Manager) TryLockRestore() bool {
	return m.restoreLock.TryLock()
}

// UnlockRestore releases the lock taken by TryLockRestore.
func (m *Manager) UnlockRestore() {
	m.restoreLock.Unlock()
}

// New returns a manager. An invalid configuration is reported by SetConfig
// errors and leaves backups unconfigured.
func New(cfg *Config, deps *Deps) *Manager {
	if deps.Logger == nil {
		deps.Logger = hclog.NewNullLogger()
	}
	m := &Manager{
		logger:   deps.Logger.Named("backup"),
		deps:     deps,
		wakeCh:   make(chan struct{}, 1),
		failures: make(map[string]int),
	}
	if err := m.SetConfig(cfg); err != nil {
		m.logger.Error("invalid backup configuration", "error", err)
	}
	return m
}

// SetConfig replaces the configuration, for example on a reload.
func (m *Manager) SetConfig(cfg *Config) error {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	cfg = cfg.Copy()

	var dests []Destination
	err := cfg.Validate()
	if err == nil {
		for _, dcfg := range cfg.Destinations {
			d, derr := NewDestination(dcfg, cfg, m.deps.Region)
			if derr != nil {
				err = fmt.Errorf("destination %q: %w", dcfg.Name, derr)
				dests = nil
				break
			}
			if dd, ok := d.(*dirDestination); ok {
				dd.server = true
			}
			dests = append(dests, d)
		}
	}

	m.mu.Lock()
	m.cfg = cfg
	m.dests = dests
	m.destsErr = err
	m.mu.Unlock()

	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
	return err
}

// Config returns the current configuration.
func (m *Manager) Config() *Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *Manager) configAndDestinations() (*Config, []Destination, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.destsErr != nil {
		return m.cfg, nil, fmt.Errorf("invalid backup configuration: %w", m.destsErr)
	}
	if !m.cfg.Configured() {
		return m.cfg, nil, errors.New("no backup destination is configured")
	}
	return m.cfg, m.dests, nil
}

// Destination returns the named destination.
func (m *Manager) Destination(name string) (Destination, error) {
	_, dests, err := m.configAndDestinations()
	if err != nil {
		return nil, err
	}
	for _, d := range dests {
		if d.Name() == name {
			return d, nil
		}
	}
	return nil, fmt.Errorf("unknown backup destination %q", name)
}

// SetLeader starts or stops the scheduler.
func (m *Manager) SetLeader(leader bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if leader == m.leader {
		return
	}
	m.leader = leader
	if leader {
		m.ctx, m.cancel = context.WithCancel(context.Background())
		go m.run(m.ctx)
	} else {
		m.cancel()
		m.ctx, m.cancel = nil, nil
	}
}

// leaderContext returns the context of the current leadership.
func (m *Manager) leaderContext() (context.Context, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.leader {
		return nil, errors.New("backups can only be taken on the leader")
	}
	return m.ctx, nil
}

// run is the scheduler loop of the leader.
func (m *Manager) run(ctx context.Context) {
	cleaned := false
	notReadyLogged := false
	var checked *Config
	for {
		wait := 10 * time.Second
		cfg := m.Config()

		if checked != cfg && cfg.Configured() {
			m.checkSharedStorage()
			checked = cfg
		}

		switch {
		case !cfg.Enabled || !cfg.Configured():
		case !m.deps.ServersReady():
			if !notReadyLogged {
				m.logger.Warn("scheduled backups wait until all servers run a Norca version that supports them")
				notReadyLogged = true
			}
		case cfg.IncludeKeyring && m.deps.KeyringReady != nil && !m.deps.KeyringReady():
			wait = time.Second
		default:
			if !cleaned {
				m.abortIncompleteUploads(ctx)
				cleaned = true
			}
			records, err := m.deps.State().BackupsList(nil)
			if err != nil {
				m.logger.Error("failed to read backup records", "error", err)
				break
			}
			now := time.Now()
			m.emitAgeMetric(records, now)
			st := cfg.schedule(records, m.deps.Region, now)
			if !now.Before(st.due) {
				if _, err := m.save(ctx, SaveOptions{Trigger: structs.BackupTriggerSchedule}); err != nil {
					m.logger.Error("scheduled backup failed", "error", err)
				}
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if d := time.Until(st.due); d < wait {
				wait = d
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.wakeCh:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// abortIncompleteUploads aborts the multipart uploads a previous leader left
// behind.
func (m *Manager) abortIncompleteUploads(ctx context.Context) {
	_, dests, err := m.configAndDestinations()
	if err != nil {
		return
	}
	m.runLock.Lock()
	defer m.runLock.Unlock()
	for _, d := range dests {
		n, err := d.AbortIncompleteUploads(ctx, "")
		if err != nil {
			m.logger.Warn("failed to abort incomplete uploads", "destination", d.Name(), "error", err)
			continue
		}
		if n > 0 {
			m.logger.Info("aborted incomplete backup uploads", "destination", d.Name(), "count", n)
		}
	}
}

// checkSharedStorage warns when the newest backup that the records list on a
// directory destination is missing there: the directory is not the same
// storage on every server of the region, and the new leader can't read the
// backups the previous one wrote.
func (m *Manager) checkSharedStorage() {
	_, dests, err := m.configAndDestinations()
	if err != nil {
		return
	}
	records, err := m.deps.State().BackupsList(nil)
	if err != nil {
		return
	}
	for _, dest := range dests {
		d, ok := dest.(*dirDestination)
		if !ok {
			continue
		}
		var newest *structs.BackupRecord
		for _, r := range records {
			st := r.Destination(d.name)
			if !r.Usable() || st == nil || st.Status != structs.BackupDestinationStatusVerified {
				continue
			}
			if newest == nil || r.StartedAt.After(newest.StartedAt) {
				newest = r
			}
		}
		if newest == nil {
			continue
		}
		if found, err := d.exists(newest.Path); err == nil && !found {
			m.logger.Warn("backup missing on a directory destination", "destination", d.name, "id", newest.ID,
				"error", sharedStorageHint(d, newest))
		}
	}
}

func (m *Manager) emitAgeMetric(records []*structs.BackupRecord, now time.Time) {
	var last time.Time
	for _, b := range records {
		if b.Usable() && b.CompletedAt.After(last) {
			last = b.CompletedAt
		}
	}
	if last.IsZero() {
		return
	}
	metrics.SetGauge([]string{"nomad", "backup", "last_success_age_seconds"}, float32(now.Sub(last).Seconds()))
}

// Save takes a backup on the leader and waits for it. With a Started
// callback the caller learns the ID as soon as the snapshot is taken (or, for
// a backup an observer takes, as soon as it is asked).
func (m *Manager) Save(opts SaveOptions) (*structs.BackupRecord, error) {
	ctx, err := m.leaderContext()
	if err != nil {
		return nil, err
	}
	if opts.Trigger == "" {
		opts.Trigger = structs.BackupTriggerManual
	}
	return m.save(ctx, opts)
}

// ErrDelegateRefused is wrapped by the errors of a delegated backup that
// nothing was started for.
var ErrDelegateRefused = errors.New("the observer refused the backup")

// DelegateRequest asks an observer to take a backup for the leader.
type DelegateRequest struct {
	// ID is the ID the leader gave the backup.
	ID      string
	Trigger string
	Labels  map[string]string
	Pin     bool

	// Fingerprint is the fingerprint of the leader's backup configuration;
	// the observer takes the backup only with the same one.
	Fingerprint string
}

func (m *Manager) save(ctx context.Context, opts SaveOptions) (*structs.BackupRecord, error) {
	cfg, dests, err := m.configAndDestinations()
	if err != nil {
		return nil, err
	}
	if !m.deps.ServersReady() {
		return nil, errors.New("all servers must run a Norca version that supports backups")
	}

	m.runLock.Lock()
	defer m.runLock.Unlock()

	// The pre-restore backup must hold the last state: the leader takes it.
	if cfg.PreferObserver && m.deps.Delegate != nil && opts.Trigger != structs.BackupTriggerPreRestore {
		if record, done, err := m.delegate(ctx, cfg, dests, &opts); done {
			return record, err
		}
	}

	tctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	return m.finish(ctx, cfg, dests, m.take(tctx, cfg, dests, opts, ""))
}

// delegate asks an observer to take the backup. It reports done when the
// backup is finished (or recorded as failed); otherwise the leader takes it
// itself, with the ID in opts.ID if one was given to the caller already.
func (m *Manager) delegate(ctx context.Context, cfg *Config, dests []Destination, opts *SaveOptions) (*structs.BackupRecord, bool, error) {
	index, _ := m.deps.State().LatestIndex()
	start := time.Now().UTC()
	id := NewID(start, index)
	if opts.Started != nil {
		opts.Started(id)
		opts.Started = nil
	}
	opts.ID = id

	dctx, cancel := context.WithTimeout(ctx, cfg.Timeout+time.Minute)
	defer cancel()
	record, err := m.deps.Delegate(dctx, &DelegateRequest{
		ID:          id,
		Trigger:     opts.Trigger,
		Labels:      maps.Clone(opts.Labels),
		Pin:         opts.Pin,
		Fingerprint: cfg.Fingerprint(),
	})
	switch {
	case errors.Is(err, ErrDelegateRefused):
		m.logger.Info("no observer takes the backup, the leader takes it", "id", id, "reason", err)
		return nil, false, nil
	case err != nil:
		// An observer may still be uploading under this ID: record it as
		// failed and take the backup under a new one.
		m.logger.Warn("the observer did not finish the backup, the leader takes a new one", "id", id, "error", err)
		failed := &structs.BackupRecord{
			ID: id, Region: m.deps.Region, AuthoritativeRegion: m.deps.AuthoritativeRegion,
			Trigger: opts.Trigger, StartedAt: start, NorcaVersion: version.NorcaVersion,
			NomadVersion: version.Version, Labels: maps.Clone(opts.Labels),
		}
		failed.Path, _ = PathFor(id)
		m.finishFailed(failed, fmt.Errorf("the observer did not finish the backup: %w", err))
		opts.ID = ""
		return nil, false, nil
	}
	if err := m.checkDelegated(record, id, dests); err != nil {
		m.logger.Warn("invalid record of an observer backup, the leader takes a new one", "id", id, "error", err)
		opts.ID = ""
		return nil, false, nil
	}
	if record.Status == structs.BackupStatusFailed {
		// The observer removed what it uploaded: the leader can use the ID.
		m.logger.Warn("the backup on the observer failed, the leader takes it", "id", id,
			"observer", record.TakenBy, "error", record.StatusDescription)
		return nil, false, nil
	}
	metrics.IncrCounter([]string{"nomad", "backup", "taken_by_observer"}, 1)
	if m.deps.TakenByObserver != nil {
		m.deps.TakenByObserver()
	}
	record, err = m.finish(ctx, cfg, dests, record)
	return record, true, err
}

// checkDelegated checks the record an observer returned.
func (m *Manager) checkDelegated(record *structs.BackupRecord, id string, dests []Destination) error {
	if record == nil {
		return errors.New("no record")
	}
	expected, _ := PathFor(id)
	if record.ID != id || record.Path != expected || record.Region != m.deps.Region {
		return fmt.Errorf("record %q (path %q, region %q) is not backup %q of region %q",
			record.ID, record.Path, record.Region, id, m.deps.Region)
	}
	for _, st := range record.Destinations {
		if !slices.ContainsFunc(dests, func(d Destination) bool { return d.Name() == st.Name }) {
			return fmt.Errorf("unknown destination %q", st.Name)
		}
	}
	return nil
}

// TakeDelegated takes a backup for the leader on this server (an observer)
// and returns its record without writing it to Raft; the leader writes it
// and applies the retention. Errors that wrap ErrDelegateRefused are
// returned before anything is started.
func (m *Manager) TakeDelegated(ctx context.Context, req *DelegateRequest) (*structs.BackupRecord, error) {
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrDelegateRefused, fmt.Sprintf(format, a...))
	}
	cfg, dests, err := m.configAndDestinations()
	if err != nil {
		return nil, refuse("%v", err)
	}
	if fp := cfg.Fingerprint(); fp != req.Fingerprint {
		return nil, refuse("its backup configuration differs from the leader's (destinations, encryption or keyring)")
	}
	if expected, err := PathFor(req.ID); err != nil || expected == "" {
		return nil, refuse("invalid backup ID %q", req.ID)
	}
	if !m.deps.ServersReady() {
		return nil, refuse("not all servers run a Norca version that supports backups")
	}
	if cfg.IncludeKeyring && m.deps.KeyringReady != nil && !m.deps.KeyringReady() {
		return nil, refuse("its keyring is not ready")
	}
	if !m.runLock.TryLock() {
		return nil, refuse("it is taking another backup")
	}
	defer m.runLock.Unlock()

	tctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	opts := SaveOptions{Trigger: req.Trigger, Labels: req.Labels, Pin: req.Pin, ID: req.ID}
	record := m.take(tctx, cfg, dests, opts, req.ID)
	if record.Status == structs.BackupStatusFailed {
		m.logger.Warn("backup for the leader failed", "id", record.ID, "error", record.StatusDescription)
	} else {
		m.logger.Info("backup for the leader taken", "id", record.ID, "status", record.Status)
	}
	return record, nil
}

// take takes a backup: the snapshot, the keyring and the other objects,
// uploaded to every destination and verified, with the manifest written last.
// It returns the record with its status; what was uploaded to a destination
// that failed is removed again. It writes nothing to Raft. id, if set, is the
// ID of the backup; otherwise it is made from the snapshot index.
func (m *Manager) take(ctx context.Context, cfg *Config, dests []Destination, opts SaveOptions, id string) *structs.BackupRecord {
	if id == "" {
		id = opts.ID
	}
	start := time.Now().UTC()
	record := &structs.BackupRecord{
		Region:              m.deps.Region,
		AuthoritativeRegion: m.deps.AuthoritativeRegion,
		Trigger:             opts.Trigger,
		StartedAt:           start,
		NorcaVersion:        version.NorcaVersion,
		NomadVersion:        version.Version,
		TakenBy:             m.deps.ServerName,
		Labels:              maps.Clone(opts.Labels),
		Pinned:              opts.Pin,
	}
	failed := func(err error) *structs.BackupRecord {
		record.Status = structs.BackupStatusFailed
		record.StatusDescription = err.Error()
		record.CompletedAt = time.Now().UTC()
		return record
	}

	snap, err := m.deps.Snapshot()
	if err != nil {
		if id == "" {
			index, _ := m.deps.State().LatestIndex()
			id = NewID(start, index)
		}
		record.ID = id
		record.Path, _ = PathFor(record.ID)
		return failed(fmt.Errorf("failed to take the snapshot: %w", err))
	}
	defer snap.Close()

	if id == "" {
		id = NewID(start, snap.Index())
	}
	record.ID = id
	record.Path, _ = PathFor(record.ID)
	record.RaftIndex = snap.Index()
	record.RaftTerm = snap.Term()
	if opts.Started != nil {
		opts.Started(record.ID)
	}
	record.Summary = Summarize(m.deps.State())
	record.Seals = SealsOf(m.deps.State())

	var keyring []byte
	if cfg.IncludeKeyring {
		keys, err := m.deps.Keyring()
		if err == nil {
			keyring, err = EncodeKeyring(keys)
		}
		if err != nil {
			return failed(fmt.Errorf("failed to read the keyring: %w", err))
		}
		for _, k := range keys {
			record.KeyIDs = append(record.KeyIDs, k.Meta.KeyID)
		}
		slices.Sort(record.KeyIDs)
	}
	var coreKey []byte
	if cfg.IncludeKeyring && m.deps.SecretStoreKey != nil {
		key, err := m.deps.SecretStoreKey()
		if err == nil && key != nil {
			coreKey, err = EncodeSecretStoreKey(key)
			clear(key)
		}
		if err != nil {
			// The backup stays usable; its secret store can be recovered
			// with the shares of its seal.
			m.logger.Warn("the backup does not include the key of the secret store", "id", record.ID, "error", err)
		}
	}

	var consulKV []byte
	if m.deps.ConsulKV != nil {
		var err error
		if consulKV, err = m.deps.ConsulKV(ctx); err != nil {
			return failed(fmt.Errorf("failed to export the Consul KV: %w", err))
		}
	}

	// Upload to every destination in parallel; one failing destination does
	// not stop the others.
	statuses := make([]*structs.BackupDestinationStatus, len(dests))
	var wg sync.WaitGroup
	for i, d := range dests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = m.upload(ctx, cfg, d, record, snap, keyring, coreKey, consulKV)
		}()
	}
	wg.Wait()
	record.Destinations = statuses
	record.CompletedAt = time.Now().UTC()

	// The manifest makes the backup visible in the store; write it last.
	for i, st := range statuses {
		if st.Status != structs.BackupDestinationStatusVerified {
			continue
		}
		if err := m.writeManifest(ctx, dests[i], record, st, true); err != nil {
			st.Status = structs.BackupDestinationStatusFailed
			st.Error = err.Error()
		}
	}

	var verified int
	var errs []string
	for i, st := range statuses {
		if st.Status == structs.BackupDestinationStatusVerified {
			verified++
			continue
		}
		errs = append(errs, fmt.Sprintf("%s: %s", st.Name, st.Error))
		// Remove what was uploaded; without a manifest it is not a backup.
		if err := dests[i].DeletePrefix(ctx, record.Path+"/"); err != nil {
			m.logger.Warn("failed to clean up a failed backup", "destination", st.Name, "error", err)
		}
	}

	switch {
	case verified == len(statuses):
		record.Status = structs.BackupStatusComplete
	case verified > 0:
		record.Status = structs.BackupStatusPartial
		record.StatusDescription = "failed destinations: " + joinErrors(errs)
	default:
		return failed(errors.New(joinErrors(errs)))
	}
	return record
}

// finish writes the record of a backup to Raft, counts the outcome per
// destination and applies the retention. A failed backup is recorded and
// returned with its error.
func (m *Manager) finish(ctx context.Context, cfg *Config, dests []Destination, record *structs.BackupRecord) (*structs.BackupRecord, error) {
	for _, st := range record.Destinations {
		labels := []metrics.Label{{Name: "destination", Value: st.Name}}
		if st.Status == structs.BackupDestinationStatusVerified {
			m.failures[st.Name] = 0
			if obj := st.Objects[structs.BackupObjectState]; obj != nil {
				metrics.SetGaugeWithLabels([]string{"nomad", "backup", "size_bytes"}, float32(obj.Size), labels)
			}
			metrics.SetGaugeWithLabels([]string{"nomad", "backup", "duration_seconds"}, float32(st.Duration.Seconds()), labels)
			continue
		}
		metrics.IncrCounterWithLabels([]string{"nomad", "backup", "failures_total"}, 1, labels)
		m.failures[st.Name]++
		if m.failures[st.Name] >= 3 {
			m.logger.Error("backups to destination keep failing", "destination", st.Name,
				"consecutive_failures", m.failures[st.Name])
		}
	}
	if record.Status == structs.BackupStatusFailed {
		return m.finishFailed(record, errors.New(record.StatusDescription))
	}

	if _, err := m.deps.Apply(structs.BackupUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupUpsertRequest{Backups: []*structs.BackupRecord{record}}); err != nil {
		return nil, fmt.Errorf("backup %s was uploaded but could not be recorded: %w", record.ID, err)
	}
	metrics.SetGauge([]string{"nomad", "backup", "last_success_age_seconds"}, 0)
	m.logger.Info("backup completed", "id", record.ID, "status", record.Status, "taken_by", record.TakenBy,
		"destinations", record.VerifiedDestinations(), "duration", record.CompletedAt.Sub(record.StartedAt))

	pctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := m.prune(pctx, cfg, dests); err != nil {
		m.logger.Warn("backup retention failed", "error", err)
	}
	return record, nil
}

func joinErrors(errs []string) string {
	var buf bytes.Buffer
	for i, e := range errs {
		if i > 0 {
			buf.WriteString("; ")
		}
		buf.WriteString(e)
	}
	return buf.String()
}

// finishFailed records a failed backup and returns its error.
func (m *Manager) finishFailed(record *structs.BackupRecord, err error) (*structs.BackupRecord, error) {
	record.Status = structs.BackupStatusFailed
	record.StatusDescription = err.Error()
	record.CompletedAt = time.Now().UTC()
	metrics.IncrCounterWithLabels([]string{"nomad", "backup", "failures_total"}, 1,
		[]metrics.Label{{Name: "destination", Value: "all"}})
	if _, aerr := m.deps.Apply(structs.BackupUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupUpsertRequest{Backups: []*structs.BackupRecord{record}}); aerr != nil {
		m.logger.Error("failed to record a failed backup", "id", record.ID, "error", aerr)
	}
	return record, fmt.Errorf("backup %s failed: %w", record.ID, err)
}

// upload encrypts and uploads the objects of a backup to a destination and
// verifies them.
func (m *Manager) upload(ctx context.Context, cfg *Config, dest Destination, record *structs.BackupRecord,
	snap Snapshot, keyring, coreKey, consulKV []byte) *structs.BackupDestinationStatus {

	start := time.Now()
	st := &structs.BackupDestinationStatus{
		Name:     dest.Name(),
		Location: dest.Location(record.Path + "/"),
		Objects:  make(map[string]*structs.BackupObject),
	}
	fail := func(err error) *structs.BackupDestinationStatus {
		st.Status = structs.BackupDestinationStatusFailed
		st.Error = err.Error()
		st.Duration = time.Since(start)
		m.logger.Warn("backup upload failed", "id", record.ID, "destination", st.Name, "error", err)
		return st
	}

	enc, err := NewEncryptor(cfg.EncryptionFor(cfg.Destination(dest.Name())))
	if err != nil {
		return fail(err)
	}
	st.EncryptionMode = enc.Mode()
	st.RecipientsFingerprint = enc.Fingerprint()

	r, err := snap.NewReader()
	if err != nil {
		return fail(err)
	}
	obj, err := putEncrypted(ctx, dest, path.Join(record.Path, StateKey), r, enc, cfg.VerifyChecksum)
	if err != nil {
		return fail(err)
	}
	obj.Key = StateKey
	st.Objects[structs.BackupObjectState] = obj

	if keyring != nil {
		obj, err := putEncrypted(ctx, dest, path.Join(record.Path, KeyringKey), bytes.NewReader(keyring), enc, cfg.VerifyChecksum)
		if err != nil {
			return fail(err)
		}
		obj.Key = KeyringKey
		st.Objects[structs.BackupObjectKeyring] = obj
	}
	if coreKey != nil {
		obj, err := putEncrypted(ctx, dest, path.Join(record.Path, SecretStoreKeyKey), bytes.NewReader(coreKey), enc, cfg.VerifyChecksum)
		if err != nil {
			return fail(err)
		}
		obj.Key = SecretStoreKeyKey
		st.Objects[structs.BackupObjectSecretStoreKey] = obj
	}
	if consulKV != nil {
		obj, err := putEncrypted(ctx, dest, path.Join(record.Path, ConsulKVKey), bytes.NewReader(consulKV), enc, cfg.VerifyChecksum)
		if err != nil {
			return fail(err)
		}
		obj.Key = ConsulKVKey
		st.Objects[structs.BackupObjectConsulKV] = obj
	}

	st.Status = structs.BackupDestinationStatusVerified
	st.Duration = time.Since(start)
	return st
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// putEncrypted streams src through the encryptor into a multipart upload,
// then verifies the stored size (and with verify, the SHA-256 of a fresh
// download). Memory use is bounded by the part buffers.
func putEncrypted(ctx context.Context, dest Destination, key string, src io.Reader, enc Encryptor, verify bool) (*structs.BackupObject, error) {
	pr, pw := io.Pipe()
	hash := sha256.New()
	counter := &countingWriter{}
	done := make(chan error, 1)
	go func() {
		w, err := enc.Encrypt(io.MultiWriter(pw, hash, counter))
		if err == nil {
			_, err = io.Copy(w, src)
			if err == nil {
				err = w.Close()
			}
		}
		pw.CloseWithError(err)
		done <- err
	}()

	putErr := dest.Put(ctx, key, pr, -1, "application/octet-stream")
	pr.CloseWithError(errors.New("upload aborted"))
	encErr := <-done
	if putErr != nil {
		return nil, putErr
	}
	if encErr != nil {
		return nil, fmt.Errorf("failed to encrypt %s: %w", key, encErr)
	}

	obj := &structs.BackupObject{Size: counter.n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	size, err := dest.Size(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to verify %s: %w", key, err)
	}
	if size != obj.Size {
		return nil, fmt.Errorf("verification of %s failed: stored size %d, uploaded %d", dest.Location(key), size, obj.Size)
	}
	if verify {
		rc, err := dest.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("failed to verify %s: %w", key, err)
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to verify %s: %w", key, err)
		}
		if sum := hex.EncodeToString(h.Sum(nil)); sum != obj.SHA256 {
			return nil, fmt.Errorf("verification of %s failed: checksum mismatch", dest.Location(key))
		}
	}
	return obj, nil
}

// writeManifest writes the manifest of a backup, and with latest the
// latest.json pointer, to a destination.
func (m *Manager) writeManifest(ctx context.Context, dest Destination, record *structs.BackupRecord,
	st *structs.BackupDestinationStatus, latest bool) error {

	buf, err := json.MarshalIndent(NewManifest(record, st), "", "  ")
	if err != nil {
		return err
	}
	if err := dest.Put(ctx, path.Join(record.Path, ManifestKey), bytes.NewReader(buf), int64(len(buf)), "application/json"); err != nil {
		return err
	}
	if !latest {
		return nil
	}
	buf, err = json.MarshalIndent(&Latest{ID: record.ID, Path: record.Path + "/", CreatedAt: record.CompletedAt}, "", "  ")
	if err != nil {
		return err
	}
	return dest.Put(ctx, LatestKey, bytes.NewReader(buf), int64(len(buf)), "application/json")
}

// Update writes a changed backup record (pin, labels) to Raft and to the
// manifests of the backup.
func (m *Manager) Update(record *structs.BackupRecord) (uint64, error) {
	ctx, err := m.leaderContext()
	if err != nil {
		return 0, err
	}
	index, err := m.deps.Apply(structs.BackupUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupUpsertRequest{Backups: []*structs.BackupRecord{record}})
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for _, st := range record.Destinations {
		if st.Status != structs.BackupDestinationStatusVerified {
			continue
		}
		dest, err := m.Destination(st.Name)
		if err != nil {
			m.logger.Warn("can't update the manifest of a backup", "id", record.ID, "destination", st.Name, "error", err)
			continue
		}
		if err := m.writeManifest(ctx, dest, record, st, false); err != nil {
			m.logger.Warn("failed to update the manifest of a backup", "id", record.ID, "destination", st.Name, "error", err)
		}
	}
	return index, nil
}

// Delete deletes the objects of a backup on the destinations that allow
// pruning, then its record. The record stays if an object can't be deleted.
func (m *Manager) Delete(record *structs.BackupRecord) (uint64, error) {
	ctx, err := m.leaderContext()
	if err != nil {
		return 0, err
	}
	cfg, dests, err := m.configAndDestinations()
	if err != nil {
		return 0, err
	}
	m.runLock.Lock()
	defer m.runLock.Unlock()
	return m.deleteLocked(ctx, cfg, dests, record, true)
}

// deleteLocked deletes a backup. A backup that its record lists on a
// directory destination but that is missing there is not deleted by the
// retention (the directory may not be shared by the servers, see
// sharedStorageHint); a manual deletion removes its record with a warning.
func (m *Manager) deleteLocked(ctx context.Context, cfg *Config, dests []Destination, record *structs.BackupRecord, manual bool) (uint64, error) {
	// Never delete outside the backup's own folder.
	expected, err := PathFor(record.ID)
	if err != nil || record.Path != expected {
		return 0, fmt.Errorf("backup %s has an unexpected path %q", record.ID, record.Path)
	}
	// The destinations to delete from: those that allow pruning. A backup
	// missing on a directory destination is checked before anything is
	// deleted.
	var targets []Destination
	for _, st := range record.Destinations {
		dcfg := cfg.Destination(st.Name)
		if dcfg == nil || !dcfg.Prune {
			continue
		}
		for _, d := range dests {
			if d.Name() != st.Name {
				continue
			}
			if dd, ok := d.(*dirDestination); ok && st.Status == structs.BackupDestinationStatusVerified {
				found, err := dd.exists(record.Path)
				if err != nil {
					return 0, fmt.Errorf("failed to delete backup %s from %s: %w", record.ID, st.Name, err)
				}
				if !found {
					if !manual {
						return 0, fmt.Errorf("failed to delete backup %s from %s: %s", record.ID, st.Name,
							sharedStorageHint(dd, record))
					}
					m.logger.Warn("deleting the record of a backup missing on a directory destination",
						"id", record.ID, "destination", st.Name, "error", sharedStorageHint(dd, record))
					continue
				}
			}
			targets = append(targets, d)
		}
	}
	for _, d := range targets {
		if err := d.DeletePrefix(ctx, record.Path+"/"); err != nil {
			return 0, fmt.Errorf("failed to delete backup %s from %s: %w", record.ID, d.Name(), err)
		}
	}
	return m.deps.Apply(structs.BackupDeleteRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupDeleteRequest{IDs: []string{record.ID}})
}

// prune deletes the backups the retention expires. Only the leader of the
// region deletes, and only within its own prefix.
func (m *Manager) prune(ctx context.Context, cfg *Config, dests []Destination) error {
	records, err := m.deps.State().BackupsList(nil)
	if err != nil {
		return err
	}
	retention := cfg.Retention
	if retention == nil {
		retention = DefaultRetention()
	}
	var errs []string
	for _, record := range Expired(records, retention) {
		if _, err := m.deleteLocked(ctx, cfg, dests, record, false); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		m.logger.Info("backup expired by retention", "id", record.ID)
	}
	if len(errs) > 0 {
		return errors.New(joinErrors(errs))
	}
	return nil
}

// RetentionOrDefault returns the effective retention.
func (c *Config) RetentionOrDefault() *RetentionConfig {
	if c.Retention == nil {
		return DefaultRetention()
	}
	return c.Retention
}

// ReadDestination picks the destination to read a backup from: the named
// one, or the first one the backup was verified on.
func (m *Manager) ReadDestination(record *structs.BackupRecord, name string) (Destination, *structs.BackupDestinationStatus, error) {
	for _, st := range record.Destinations {
		if st.Status != structs.BackupDestinationStatusVerified || (name != "" && st.Name != name) {
			continue
		}
		dest, err := m.Destination(st.Name)
		if err != nil {
			if name != "" {
				return nil, nil, err
			}
			continue
		}
		return dest, st, nil
	}
	if name != "" {
		return nil, nil, fmt.Errorf("backup %s is not available on destination %q", record.ID, name)
	}
	return nil, nil, fmt.Errorf("backup %s is not available on any configured destination", record.ID)
}

// LoadRecord reads the manifest of a backup that is not in the
// backup_status table from a destination (the first one by default).
func (m *Manager) LoadRecord(ctx context.Context, id, destName string) (*structs.BackupRecord, error) {
	if _, err := PathFor(id); err != nil {
		return nil, err
	}
	cfg, _, err := m.configAndDestinations()
	if err != nil {
		return nil, err
	}
	names := cfg.DestinationNames()
	if destName != "" {
		names = []string{destName}
	}
	var lastErr error
	for _, name := range names {
		dest, err := m.Destination(name)
		if err != nil {
			return nil, err
		}
		record, err := ReadRecord(ctx, dest, id)
		if errors.Is(err, ErrNotFound) {
			lastErr = err
			continue
		}
		if err != nil {
			return nil, err
		}
		return record, nil
	}
	if errors.Is(lastErr, ErrNotFound) {
		return nil, fmt.Errorf("backup %s not found", id)
	}
	return nil, lastErr
}

// ReadRecord reads the manifest of a backup from a destination and returns
// the backup record it describes. A missing manifest is ErrNotFound.
func ReadRecord(ctx context.Context, dest Destination, id string) (*structs.BackupRecord, error) {
	backupPath, err := PathFor(id)
	if err != nil {
		return nil, err
	}
	rc, err := dest.Get(ctx, path.Join(backupPath, ManifestKey))
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	err = json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&manifest)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("invalid manifest of backup %s: %w", id, err)
	}
	if manifest.ID != id {
		return nil, fmt.Errorf("manifest of backup %s has ID %q", id, manifest.ID)
	}
	return manifest.Record(dest, backupPath), nil
}

// ReadLatest reads the latest.json pointer of a destination and returns
// the ID of the most recent successful backup.
func ReadLatest(ctx context.Context, dest Destination) (string, error) {
	rc, err := dest.Get(ctx, LatestKey)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var latest Latest
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&latest); err != nil {
		return "", fmt.Errorf("invalid %s: %w", LatestKey, err)
	}
	if _, _, err := ParseID(latest.ID); err != nil {
		return "", err
	}
	return latest.ID, nil
}

// Open returns the decrypted content of a backup object. The SHA-256 of the
// stored object is checked against the record when the content has been
// read to the end; a mismatch is returned instead of io.EOF.
func (m *Manager) Open(ctx context.Context, record *structs.BackupRecord, destName, object string, ids Identities) (io.ReadCloser, error) {
	dest, st, err := m.ReadDestination(record, destName)
	if err != nil {
		return nil, err
	}
	return OpenObject(ctx, dest, record, st, object, ids)
}

// OpenObject returns the decrypted content of an object of a backup on a
// destination, as Manager.Open. st is the status of the backup on the
// destination, which lists its objects and their checksums.
func OpenObject(ctx context.Context, dest Destination, record *structs.BackupRecord,
	st *structs.BackupDestinationStatus, object string, ids Identities) (io.ReadCloser, error) {

	obj := st.Objects[object]
	if obj == nil {
		return nil, fmt.Errorf("backup %s has no %s object: %w", record.ID, object, ErrNotFound)
	}
	raw, err := dest.Get(ctx, path.Join(record.Path, obj.Key))
	if err != nil {
		return nil, missingError(dest, record, err)
	}
	hash := sha256.New()
	counter := &countingWriter{}
	tee := io.TeeReader(raw, io.MultiWriter(hash, counter))
	plain, err := ids.decrypt(tee)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return &verifyingReader{plain: plain, tee: tee, raw: raw, hash: hash, counter: counter, obj: obj}, nil
}

type verifyingReader struct {
	plain   io.Reader
	tee     io.Reader
	raw     io.ReadCloser
	hash    interface{ Sum([]byte) []byte }
	counter *countingWriter
	obj     *structs.BackupObject
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.plain.Read(p)
	if err != io.EOF {
		return n, err
	}
	// Read any trailing bytes so the checksum covers the whole object.
	if _, derr := io.Copy(io.Discard, v.tee); derr != nil {
		return n, derr
	}
	if v.obj.SHA256 != "" && hex.EncodeToString(v.hash.Sum(nil)) != v.obj.SHA256 {
		return n, errors.New("backup object checksum mismatch")
	}
	if v.obj.Size > 0 && v.counter.n != v.obj.Size {
		return n, errors.New("backup object size mismatch")
	}
	return n, io.EOF
}

func (v *verifyingReader) Close() error {
	return v.raw.Close()
}

// PresignURL returns a pre-signed download URL of a backup object. Without
// a destination name it picks the first destination the backup was verified
// on that can sign URLs: directory destinations can't (ErrNoDownloadURL).
func (m *Manager) PresignURL(ctx context.Context, record *structs.BackupRecord, destName, object string, expiry time.Duration) (string, error) {
	dest, st, err := m.ReadDestination(record, destName)
	if err != nil {
		return "", err
	}
	if _, ok := dest.(*dirDestination); ok && destName == "" {
		for _, s := range record.Destinations {
			if s.Status != structs.BackupDestinationStatusVerified {
				continue
			}
			if d, err := m.Destination(s.Name); err == nil {
				if _, isDir := d.(*dirDestination); !isDir {
					dest, st = d, s
					break
				}
			}
		}
	}
	key := ManifestKey
	if object != structs.BackupObjectManifest {
		obj := st.Objects[object]
		if obj == nil {
			return "", fmt.Errorf("backup %s has no %s object", record.ID, object)
		}
		key = obj.Key
	}
	return dest.PresignGet(ctx, path.Join(record.Path, key), expiry)
}

// EncodeKeyring serializes root keys into the (unencrypted) keyring object
// content: gzip-compressed JSON.
func EncodeKeyring(keys []*structs.RootKey) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(keys); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// secretStoreKeyVersion is the version of the secret store key object.
const secretStoreKeyVersion = 1

// secretStoreKeyObject is the (unencrypted) content of the secret store key
// object: JSON with the key of the secret store core.
type secretStoreKeyObject struct {
	Version int    `json:"norca_secret_store_key"`
	CoreKey []byte `json:"core_key"`
}

// EncodeSecretStoreKey serializes the key of the secret store core into the
// content of the secret store key object.
func EncodeSecretStoreKey(key []byte) ([]byte, error) {
	return json.Marshal(&secretStoreKeyObject{Version: secretStoreKeyVersion, CoreKey: key})
}

// DecodeSecretStoreKey parses the secret store key object content.
func DecodeSecretStoreKey(r io.Reader) ([]byte, error) {
	var obj secretStoreKeyObject
	if err := json.NewDecoder(io.LimitReader(r, 64<<10)).Decode(&obj); err != nil {
		return nil, fmt.Errorf("invalid secret store key object: %w", err)
	}
	if obj.Version != secretStoreKeyVersion || len(obj.CoreKey) == 0 {
		return nil, fmt.Errorf("invalid secret store key object (version %d)", obj.Version)
	}
	// Read to the end so the caller's checksum covers the whole object.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, err
	}
	return obj.CoreKey, nil
}

// DecodeKeyring parses the keyring object content.
func DecodeKeyring(r io.Reader) ([]*structs.RootKey, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("invalid keyring object: %w", err)
	}
	defer zr.Close()
	var keys []*structs.RootKey
	if err := json.NewDecoder(zr).Decode(&keys); err != nil {
		return nil, fmt.Errorf("invalid keyring object: %w", err)
	}
	// Read to the end so the caller's checksum covers the whole object.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return nil, err
	}
	return keys, nil
}

// Summarize counts the objects of a state store for the manifest. The
// counts are read at about the Raft index of the snapshot.
func Summarize(store *state.StateStore) map[string]int {
	ws := memdb.NewWatchSet()
	count := func(iter memdb.ResultIterator, err error) int {
		if err != nil {
			return 0
		}
		n := 0
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			n++
		}
		return n
	}
	return map[string]int{
		"namespaces":   count(store.Namespaces(ws)),
		"jobs":         count(store.Jobs(ws)),
		"job_versions": count(store.JobVersions(ws)),
		"variables":    count(store.Variables(ws)),
		"acl_policies": count(store.ACLPolicies(ws)),
		"acl_roles":    count(store.GetACLRoles(ws)),
		"deployments":  count(store.Deployments(ws, state.SortDefault)),
		"allocations":  count(store.Allocs(ws, state.SortDefault)),
	}
}
