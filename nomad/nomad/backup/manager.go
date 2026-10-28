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

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/version"
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

	// LeaderID is the name of this server, recorded as the leader that took
	// the backup.
	LeaderID string

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
// callback the caller learns the ID as soon as the snapshot is taken.
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

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	start := time.Now().UTC()
	record := &structs.BackupRecord{
		Region:              m.deps.Region,
		AuthoritativeRegion: m.deps.AuthoritativeRegion,
		Trigger:             opts.Trigger,
		StartedAt:           start,
		NorcaVersion:        version.NorcaVersion,
		NomadVersion:        version.Version,
		LeaderID:            m.deps.LeaderID,
		Labels:              maps.Clone(opts.Labels),
		Pinned:              opts.Pin,
	}

	snap, err := m.deps.Snapshot()
	if err != nil {
		index, _ := m.deps.State().LatestIndex()
		record.ID = NewID(start, index)
		record.Path, _ = PathFor(record.ID)
		return m.finishFailed(record, fmt.Errorf("failed to take the snapshot: %w", err))
	}
	defer snap.Close()

	record.ID = NewID(start, snap.Index())
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
			return m.finishFailed(record, fmt.Errorf("failed to read the keyring: %w", err))
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

	// Upload to every destination in parallel; one failing destination does
	// not stop the others.
	statuses := make([]*structs.BackupDestinationStatus, len(dests))
	var wg sync.WaitGroup
	for i, d := range dests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = m.upload(ctx, cfg, d, record, snap, keyring, coreKey)
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
		labels := []metrics.Label{{Name: "destination", Value: st.Name}}
		if st.Status == structs.BackupDestinationStatusVerified {
			verified++
			m.failures[st.Name] = 0
			if obj := st.Objects[structs.BackupObjectState]; obj != nil {
				metrics.SetGaugeWithLabels([]string{"nomad", "backup", "size_bytes"}, float32(obj.Size), labels)
			}
			metrics.SetGaugeWithLabels([]string{"nomad", "backup", "duration_seconds"}, float32(st.Duration.Seconds()), labels)
			continue
		}
		errs = append(errs, fmt.Sprintf("%s: %s", st.Name, st.Error))
		metrics.IncrCounterWithLabels([]string{"nomad", "backup", "failures_total"}, 1, labels)
		m.failures[st.Name]++
		if m.failures[st.Name] >= 3 {
			m.logger.Error("backups to destination keep failing", "destination", st.Name,
				"consecutive_failures", m.failures[st.Name])
		}
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
		return m.finishFailed(record, errors.New(joinErrors(errs)))
	}

	if _, err := m.deps.Apply(structs.BackupUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupUpsertRequest{Backups: []*structs.BackupRecord{record}}); err != nil {
		return nil, fmt.Errorf("backup %s was uploaded but could not be recorded: %w", record.ID, err)
	}
	metrics.SetGauge([]string{"nomad", "backup", "last_success_age_seconds"}, 0)
	m.logger.Info("backup completed", "id", record.ID, "status", record.Status,
		"destinations", record.VerifiedDestinations(), "duration", record.CompletedAt.Sub(start))

	if err := m.prune(ctx, cfg, dests); err != nil {
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
	snap Snapshot, keyring, coreKey []byte) *structs.BackupDestinationStatus {

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
