// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Values of -jetstream-storage and -jetstream-compression.
const (
	StorageFile     = "file"
	StorageMemory   = "memory"
	CompressionNone = "none"
	CompressionS2   = "s2"
)

// StreamSettings are the settings of the stream the bridge creates with
// -jetstream-create. The bridge owns the stream: on every start it brings
// the settings that can change (limits, replicas, compression, direct gets)
// of an existing stream back to these. The storage and the deny flags can
// only be chosen when the stream is created.
type StreamSettings struct {
	Duplicates time.Duration
	Replicas   int
	// MaxAge and MaxBytes limit the stream, 0 for no limit; the oldest
	// messages are discarded first. MaxBytes is the size of each replica.
	MaxAge      time.Duration
	MaxBytes    int64
	Storage     string
	Compression string
	AllowDirect bool
	DenyDelete  bool
	DenyPurge   bool
}

// StreamSettings returns the settings of the stream of -jetstream-create.
func (c *Config) StreamSettings() StreamSettings {
	return StreamSettings{
		Duplicates:  c.JetStreamDuplicates,
		Replicas:    c.JetStreamReplicas,
		MaxAge:      c.JetStreamMaxAge,
		MaxBytes:    c.JetStreamMaxBytes,
		Storage:     c.JetStreamStorage,
		Compression: c.JetStreamCompression,
		AllowDirect: c.JetStreamAllowDirect,
		DenyDelete:  c.JetStreamDenyDelete,
		DenyPurge:   c.JetStreamDenyPurge,
	}
}

// StreamResult tells what EnsureStream did.
type StreamResult struct {
	Created bool
	// Updated names the settings changed on an existing stream.
	Updated []string
	// Kept names the settings of an existing stream that differ from the
	// wanted ones but can't be changed (storage, deny flags).
	Kept []string
}

// EnsureStream creates the JetStream stream name for the subjects of the
// region, or updates the settings of an existing one that can change.
func EnsureStream(ctx context.Context, js jetstream.JetStream, name, prefix, region string, set StreamSettings) (StreamResult, error) {
	return ensureStream(ctx, js, jetstream.StreamConfig{
		Name:        name,
		Description: "Norca events of region " + region,
		Subjects:    []string{prefix + "." + subjectToken(region) + ".>"},
	}, set)
}

// ensureStream creates the stream of base (name, description, subjects)
// with the settings, or updates the settings of an existing one that can
// change; the subjects of an existing stream are kept.
func ensureStream(ctx context.Context, js jetstream.JetStream, base jetstream.StreamConfig, set StreamSettings) (StreamResult, error) {
	var res StreamResult
	name := base.Name
	s, err := js.Stream(ctx, name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		cfg := base
		cfg.Storage = storageType(set.Storage)
		cfg.DenyDelete = set.DenyDelete
		cfg.DenyPurge = set.DenyPurge
		set.apply(&cfg)
		_, err = js.CreateStream(ctx, cfg)
		if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			// Created by another instance in the meantime.
			return res, nil
		}
		res.Created = err == nil
		return res, err
	}
	if err != nil {
		return res, err
	}

	info, err := s.Info(ctx)
	if err != nil {
		return res, err
	}
	cfg := info.Config
	before := cfg
	set.apply(&cfg)
	for _, d := range []struct {
		name    string
		changed bool
	}{
		{"duplicates", cfg.Duplicates != before.Duplicates},
		{"replicas", cfg.Replicas != before.Replicas},
		{"max_age", cfg.MaxAge != before.MaxAge},
		{"max_bytes", cfg.MaxBytes != before.MaxBytes},
		{"discard", cfg.Discard != before.Discard},
		{"compression", cfg.Compression != before.Compression},
		{"allow_direct", cfg.AllowDirect != before.AllowDirect},
	} {
		if d.changed {
			res.Updated = append(res.Updated, d.name)
		}
	}
	if cfg.Storage != storageType(set.Storage) {
		res.Kept = append(res.Kept, "storage")
	}
	if cfg.DenyDelete != set.DenyDelete {
		res.Kept = append(res.Kept, "deny_delete")
	}
	if cfg.DenyPurge != set.DenyPurge {
		res.Kept = append(res.Kept, "deny_purge")
	}
	if cfg.Storage == jetstream.MemoryStorage {
		// Compression is for file storage only.
		cfg.Compression = before.Compression
		res.Updated = without(res.Updated, "compression")
	}
	if len(res.Updated) == 0 {
		return res, nil
	}
	_, err = js.UpdateStream(ctx, cfg)
	return res, err
}

// apply sets the settings that can change on an existing stream.
func (set StreamSettings) apply(cfg *jetstream.StreamConfig) {
	cfg.Duplicates = set.Duplicates
	cfg.Replicas = max(set.Replicas, 1)
	cfg.MaxAge = max(set.MaxAge, 0)
	cfg.MaxBytes = set.MaxBytes
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = -1
	}
	// Full streams drop their oldest events, the bridge keeps publishing.
	cfg.Discard = jetstream.DiscardOld
	cfg.Compression = jetstream.NoCompression
	if set.Compression == CompressionS2 {
		cfg.Compression = jetstream.S2Compression
	}
	cfg.AllowDirect = set.AllowDirect
}

func storageType(s string) jetstream.StorageType {
	if s == StorageMemory {
		return jetstream.MemoryStorage
	}
	return jetstream.FileStorage
}

func without(list []string, item string) []string {
	out := list[:0]
	for _, s := range list {
		if s != item {
			out = append(out, s)
		}
	}
	return out
}

// ParseBytes parses a size: a number of bytes, or a number with the unit K,
// M, G or T (powers of 1024, like the NATS configuration), optionally
// followed by B or iB: "512MB", "1GiB", "1048576". Empty is 0.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	upper := strings.ToUpper(s)
	upper = strings.TrimSuffix(strings.TrimSuffix(upper, "B"), "I")
	mult := int64(1)
	if n := len(upper); n > 0 {
		switch upper[n-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult > 1 {
			upper = upper[:n-1]
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(upper), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return n * mult, nil
}
