// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore/ubix/export/audit"
)

// The audit trail of the secret store uses uBixVault's audit broker: every
// access is fanned out to the configured devices, and the broker is fail
// closed, so an access that cannot be recorded is refused. uBixVault's
// audit.Entry only knows the fields of its own HTTP API, so the store
// attaches the full Norca record (structs.SecretAccessEvent) to the context
// of the Log call; Norca's devices render that record.

type auditRecordKey struct{}

// WithAuditRecord returns a context carrying the audit record of an access,
// for the devices of an audit broker.
func WithAuditRecord(ctx context.Context, rec *structs.SecretAccessEvent) context.Context {
	return context.WithValue(ctx, auditRecordKey{}, rec)
}

// AuditRecordFrom returns the audit record carried by the context, or nil.
func AuditRecordFrom(ctx context.Context) *structs.SecretAccessEvent {
	rec, _ := ctx.Value(auditRecordKey{}).(*structs.SecretAccessEvent)
	return rec
}

// AuditFileConfig configures the append-only audit file of a server.
type AuditFileConfig struct {
	// Path is the file the records are appended to.
	Path string

	// RotateBytes rotates the file before it grows beyond this size; 0
	// disables size based rotation.
	RotateBytes int64

	// RotateDuration rotates the file once it is older than this; 0
	// disables time based rotation.
	RotateDuration time.Duration

	// RotateMaxFiles is the number of rotated files kept; older ones are
	// deleted. 0 keeps all of them.
	RotateMaxFiles int
}

// AuditFile is an audit device that appends one JSON line per record to a
// file and rotates it by size and age. A rotated file is renamed to
// "<path>.<UTC timestamp>". It never writes a secret value: the records do
// not contain any.
type AuditFile struct {
	cfg AuditFileConfig
	now func() time.Time

	mu     sync.Mutex
	f      *os.File
	size   int64
	opened time.Time
}

var _ audit.Device = (*AuditFile)(nil)

// NewAuditFile opens (or creates) the audit file for appending.
func NewAuditFile(cfg AuditFileConfig) (*AuditFile, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("secret store audit: no file path")
	}
	d := &AuditFile{cfg: cfg, now: time.Now}
	if err := d.open(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *AuditFile) open() error {
	// The path is operator configuration, not request input.
	f, err := os.OpenFile(d.cfg.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("secret store audit: open %q: %w", d.cfg.Path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("secret store audit: stat %q: %w", d.cfg.Path, err)
	}
	d.f, d.size, d.opened = f, info.Size(), d.now()
	return nil
}

// auditLine is the on-disk shape of a record.
type auditLine struct {
	Time      string   `json:"time"`
	Type      string   `json:"type"`
	Region    string   `json:"region,omitempty"`
	Server    string   `json:"server,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Path      string   `json:"path"`
	Version   uint64   `json:"version,omitempty"`
	Versions  []uint64 `json:"versions,omitempty"`
	Operation string   `json:"operation"`
	// Backup and BackupVersion are the source of a restored value.
	Backup        string                  `json:"backup,omitempty"`
	BackupVersion uint64                  `json:"backup_version,omitempty"`
	Result        string                  `json:"result,omitempty"`
	Error         string                  `json:"error,omitempty"`
	Accessor      *structs.SecretAccessor `json:"accessor,omitempty"`
}

// Log implements audit.Device. It renders the Norca record of the context,
// or the uBixVault entry if there is none.
func (d *AuditFile) Log(ctx context.Context, e *audit.Entry) error {
	line := auditLine{
		Time:      e.Time.UTC().Format(time.RFC3339Nano),
		Type:      e.Type,
		Path:      e.Path,
		Operation: e.Operation,
	}
	if rec := AuditRecordFrom(ctx); rec != nil {
		line.Region = rec.Region
		line.Server = rec.Server
		line.Namespace = rec.Namespace
		line.Path = rec.Path
		line.Version = rec.Version
		line.Versions = rec.Versions
		line.Backup = rec.BackupID
		line.BackupVersion = rec.BackupVersion
		line.Operation = rec.Operation
		line.Accessor = rec.Accessor
		if e.Type == "response" {
			line.Result = rec.Result
			line.Error = rec.Error
		}
	}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("secret store audit: marshal: %w", err)
	}
	data = append(data, '\n')

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return fmt.Errorf("secret store audit: %q is closed", d.cfg.Path)
	}
	if d.needsRotation(int64(len(data))) {
		if err := d.rotate(); err != nil {
			return err
		}
	}
	n, err := d.f.Write(data)
	d.size += int64(n)
	if err != nil {
		return fmt.Errorf("secret store audit: write: %w", err)
	}
	return nil
}

func (d *AuditFile) needsRotation(next int64) bool {
	if d.size == 0 {
		return false
	}
	if d.cfg.RotateBytes > 0 && d.size+next > d.cfg.RotateBytes {
		return true
	}
	return d.cfg.RotateDuration > 0 && d.now().Sub(d.opened) >= d.cfg.RotateDuration
}

// rotate renames the current file, opens a new one and removes the rotated
// files beyond RotateMaxFiles. The caller holds d.mu.
func (d *AuditFile) rotate() error {
	if err := d.f.Close(); err != nil {
		return fmt.Errorf("secret store audit: close: %w", err)
	}
	d.f = nil
	rotated := d.cfg.Path + "." + d.now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(d.cfg.Path, rotated); err != nil {
		// Keep appending to the current file rather than losing records.
		if oerr := d.open(); oerr != nil {
			return oerr
		}
		return fmt.Errorf("secret store audit: rotate: %w", err)
	}
	if err := d.open(); err != nil {
		return err
	}
	d.prune()
	return nil
}

// prune removes the oldest rotated files beyond RotateMaxFiles.
func (d *AuditFile) prune() {
	if d.cfg.RotateMaxFiles <= 0 {
		return
	}
	rotated := d.Rotated()
	for len(rotated) > d.cfg.RotateMaxFiles {
		_ = os.Remove(rotated[0])
		rotated = rotated[1:]
	}
}

// Rotated returns the paths of the rotated files, oldest first.
func (d *AuditFile) Rotated() []string {
	dir, base := filepath.Split(d.cfg.Path)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), base+".") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	// The timestamp suffix sorts chronologically.
	sort.Strings(out)
	return out
}

// Close implements audit.Device.
func (d *AuditFile) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}
