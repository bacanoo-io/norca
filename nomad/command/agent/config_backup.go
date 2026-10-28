// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"slices"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/backup"
)

// BackupConfig is the server's backup block: scheduled, encrypted backups of
// the region's Raft state and keyring into S3-compatible stores and
// directories.
//
//	backup {
//	  enabled  = true
//	  interval = "1h"
//	  destination "local" {
//	    type = "directory"
//	    path = "/var/backups/norca"
//	  }
//	  destination "primary" {
//	    type   = "s3"
//	    bucket = "norca-backup"
//	    prefix = "norca/${region}"
//	  }
//	  encryption {
//	    recipients = ["age1..."]
//	  }
//	}
type BackupConfig struct {
	// Enabled starts the scheduled backups on the leader.
	Enabled *bool `hcl:"enabled"`

	// Interval or Schedule (cron, UTC) sets the scheduled times.
	Interval string `hcl:"interval"`
	Schedule string `hcl:"schedule"`

	// Jitter bounds the per-region offset of the scheduled times.
	Jitter string `hcl:"jitter"`

	// Timeout bounds a backup run.
	Timeout string `hcl:"timeout"`

	// RetryInterval is the wait after a failed scheduled backup.
	RetryInterval string `hcl:"retry_interval"`

	// PartSize (for example "16MiB") and UploadConcurrency tune the
	// multipart upload.
	PartSize          string `hcl:"part_size"`
	UploadConcurrency int    `hcl:"upload_concurrency"`

	// IncludeKeyring adds the encrypted keyring to the backups.
	IncludeKeyring *bool `hcl:"include_keyring"`

	// VerifyChecksum downloads uploaded objects again to verify them.
	VerifyChecksum *bool `hcl:"verify_checksum"`

	// BrowseCacheTTL is how long a decrypted backup stays loaded for
	// browsing.
	BrowseCacheTTL string `hcl:"browse_cache_ttl"`

	// RequireApproval makes a second administrator approve full restores.
	// It is mandatory and can't be set to false.
	RequireApproval *bool `hcl:"require_approval"`

	// ApprovalTTL is how long a full restore request waits for approval.
	ApprovalTTL string `hcl:"approval_ttl"`

	Destinations []*BackupDestinationConfig `hcl:"destination"`
	Encryption   *BackupEncryptionConfig    `hcl:"encryption"`
	Retention    *BackupRetentionConfig     `hcl:"retention"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// BackupDestinationConfig is a backup destination: an S3-compatible store
// (type "s3", the default) or a directory (type "directory", with path).
type BackupDestinationConfig struct {
	Name           string `hcl:",key"`
	Type           string `hcl:"type"`
	Path           string `hcl:"path"`
	Endpoint       string `hcl:"endpoint"`
	Bucket         string `hcl:"bucket"`
	Prefix         string `hcl:"prefix"`
	Region         string `hcl:"region"`
	ForcePathStyle *bool  `hcl:"force_path_style"`

	AccessKey       string `hcl:"access_key"`
	SecretKey       string `hcl:"secret_key"`
	SessionToken    string `hcl:"session_token"`
	CredentialsFile string `hcl:"credentials_file"`
	Profile         string `hcl:"profile"`
	CAFile          string `hcl:"ca_file"`

	SSE    string `hcl:"sse"`
	KMSKey string `hcl:"kms_key"`

	// Encryption overrides the client-side encryption of the destination.
	Encryption *BackupEncryptionConfig `hcl:"encryption"`

	// Prune lets the retention delete on this destination (default true).
	Prune *bool `hcl:"prune"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// BackupEncryptionConfig is the client-side encryption of backups.
type BackupEncryptionConfig struct {
	Mode       string   `hcl:"mode"`
	Recipients []string `hcl:"recipients"`
	KMSKey     string   `hcl:"kms_key"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// BackupRetentionConfig is the retention of backups.
type BackupRetentionConfig struct {
	KeepHourly  *int `hcl:"keep_hourly"`
	KeepDaily   *int `hcl:"keep_daily"`
	KeepWeekly  *int `hcl:"keep_weekly"`
	KeepMonthly *int `hcl:"keep_monthly"`
	MinKeep     *int `hcl:"min_keep"`
	KeepFailed  *int `hcl:"keep_failed"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a deep copy.
func (e *BackupEncryptionConfig) Copy() *BackupEncryptionConfig {
	if e == nil {
		return nil
	}
	ne := *e
	ne.Recipients = slices.Clone(e.Recipients)
	ne.ExtraKeysHCL = slices.Clone(e.ExtraKeysHCL)
	return &ne
}

// Copy returns a deep copy.
func (d *BackupDestinationConfig) Copy() *BackupDestinationConfig {
	if d == nil {
		return nil
	}
	nd := *d
	nd.ForcePathStyle = pointer.Copy(d.ForcePathStyle)
	nd.Prune = pointer.Copy(d.Prune)
	nd.Encryption = d.Encryption.Copy()
	nd.ExtraKeysHCL = slices.Clone(d.ExtraKeysHCL)
	return &nd
}

// Copy returns a deep copy.
func (r *BackupRetentionConfig) Copy() *BackupRetentionConfig {
	if r == nil {
		return nil
	}
	nr := *r
	nr.KeepHourly = pointer.Copy(r.KeepHourly)
	nr.KeepDaily = pointer.Copy(r.KeepDaily)
	nr.KeepWeekly = pointer.Copy(r.KeepWeekly)
	nr.KeepMonthly = pointer.Copy(r.KeepMonthly)
	nr.MinKeep = pointer.Copy(r.MinKeep)
	nr.KeepFailed = pointer.Copy(r.KeepFailed)
	nr.ExtraKeysHCL = slices.Clone(r.ExtraKeysHCL)
	return &nr
}

// Copy returns a deep copy of the backup configuration.
func (b *BackupConfig) Copy() *BackupConfig {
	if b == nil {
		return nil
	}
	nb := *b
	nb.Enabled = pointer.Copy(b.Enabled)
	nb.IncludeKeyring = pointer.Copy(b.IncludeKeyring)
	nb.VerifyChecksum = pointer.Copy(b.VerifyChecksum)
	nb.RequireApproval = pointer.Copy(b.RequireApproval)
	nb.Destinations = nil
	for _, d := range b.Destinations {
		nb.Destinations = append(nb.Destinations, d.Copy())
	}
	nb.Encryption = b.Encryption.Copy()
	nb.Retention = b.Retention.Copy()
	nb.ExtraKeysHCL = slices.Clone(b.ExtraKeysHCL)
	return &nb
}

// Merge merges b into a copy of a. Destinations of b replace those of a with
// the same name; the encryption and retention blocks of b replace those of
// a field by field.
func (a *BackupConfig) Merge(b *BackupConfig) *BackupConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b == nil {
		return result
	}

	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.Interval != "" {
		result.Interval = b.Interval
	}
	if b.Schedule != "" {
		result.Schedule = b.Schedule
	}
	if b.Jitter != "" {
		result.Jitter = b.Jitter
	}
	if b.Timeout != "" {
		result.Timeout = b.Timeout
	}
	if b.RetryInterval != "" {
		result.RetryInterval = b.RetryInterval
	}
	if b.PartSize != "" {
		result.PartSize = b.PartSize
	}
	if b.UploadConcurrency != 0 {
		result.UploadConcurrency = b.UploadConcurrency
	}
	if b.IncludeKeyring != nil {
		result.IncludeKeyring = pointer.Copy(b.IncludeKeyring)
	}
	if b.VerifyChecksum != nil {
		result.VerifyChecksum = pointer.Copy(b.VerifyChecksum)
	}
	if b.BrowseCacheTTL != "" {
		result.BrowseCacheTTL = b.BrowseCacheTTL
	}
	if b.RequireApproval != nil {
		result.RequireApproval = pointer.Copy(b.RequireApproval)
	}
	if b.ApprovalTTL != "" {
		result.ApprovalTTL = b.ApprovalTTL
	}
	for _, bd := range b.Destinations {
		replaced := false
		for i, rd := range result.Destinations {
			if rd.Name == bd.Name {
				result.Destinations[i] = bd.Copy()
				replaced = true
				break
			}
		}
		if !replaced {
			result.Destinations = append(result.Destinations, bd.Copy())
		}
	}
	if b.Encryption != nil {
		if result.Encryption == nil {
			result.Encryption = b.Encryption.Copy()
		} else {
			if b.Encryption.Mode != "" {
				result.Encryption.Mode = b.Encryption.Mode
			}
			if b.Encryption.Recipients != nil {
				result.Encryption.Recipients = slices.Clone(b.Encryption.Recipients)
			}
			if b.Encryption.KMSKey != "" {
				result.Encryption.KMSKey = b.Encryption.KMSKey
			}
		}
	}
	if b.Retention != nil {
		if result.Retention == nil {
			result.Retention = b.Retention.Copy()
		} else {
			r := result.Retention
			r.KeepHourly = pointer.Merge(r.KeepHourly, b.Retention.KeepHourly)
			r.KeepDaily = pointer.Merge(r.KeepDaily, b.Retention.KeepDaily)
			r.KeepWeekly = pointer.Merge(r.KeepWeekly, b.Retention.KeepWeekly)
			r.KeepMonthly = pointer.Merge(r.KeepMonthly, b.Retention.KeepMonthly)
			r.MinKeep = pointer.Merge(r.MinKeep, b.Retention.MinKeep)
			r.KeepFailed = pointer.Merge(r.KeepFailed, b.Retention.KeepFailed)
		}
	}
	return result
}

// Redact hides the S3 secrets, for the agent self endpoint.
func (b *BackupConfig) Redact() {
	if b == nil {
		return
	}
	for _, d := range b.Destinations {
		if d.SecretKey != "" {
			d.SecretKey = "<redacted>"
		}
		if d.SessionToken != "" {
			d.SessionToken = "<redacted>"
		}
	}
}

// extraKeys removes the labels of the destination blocks from the unused
// keys and checks the nested blocks, which UnusedKeys does not descend into.
func (b *BackupConfig) extraKeys() error {
	if b == nil {
		return nil
	}
	for _, d := range b.Destinations {
		helper.RemoveEqualFold(&b.ExtraKeysHCL, d.Name)
		helper.RemoveEqualFold(&b.ExtraKeysHCL, "destination")
		if d.Encryption != nil {
			helper.RemoveEqualFold(&d.ExtraKeysHCL, "encryption")
			if err := helper.UnusedKeys(d.Encryption); err != nil {
				return fmt.Errorf("server.backup.destination.%s.encryption: %w", d.Name, err)
			}
		}
		if err := helper.UnusedKeys(d); err != nil {
			return fmt.Errorf("server.backup.destination.%s: %w", d.Name, err)
		}
	}
	return nil
}

// toServerConfig converts the agent configuration into the server's backup
// configuration, applying defaults and validating it.
func (b *BackupConfig) toServerConfig() (*backup.Config, error) {
	cfg := backup.DefaultConfig()
	if b == nil {
		return cfg, nil
	}

	if b.Enabled != nil {
		cfg.Enabled = *b.Enabled
	}
	cfg.Schedule = b.Schedule
	durations := []struct {
		name  string
		value string
		field *time.Duration
	}{
		{"interval", b.Interval, &cfg.Interval},
		{"jitter", b.Jitter, &cfg.Jitter},
		{"timeout", b.Timeout, &cfg.Timeout},
		{"retry_interval", b.RetryInterval, &cfg.RetryInterval},
		{"browse_cache_ttl", b.BrowseCacheTTL, &cfg.BrowseCacheTTL},
		{"approval_ttl", b.ApprovalTTL, &cfg.ApprovalTTL},
	}
	for _, d := range durations {
		if d.value == "" {
			continue
		}
		v, err := time.ParseDuration(d.value)
		if err != nil {
			return nil, fmt.Errorf("backup.%s can't parse time duration %s", d.name, d.value)
		}
		*d.field = v
	}
	if b.Schedule != "" && b.Interval != "" {
		return nil, fmt.Errorf("backup.interval and backup.schedule are mutually exclusive")
	}
	if b.PartSize != "" {
		size, err := humanize.ParseBytes(b.PartSize)
		if err != nil {
			return nil, fmt.Errorf("backup.part_size can't parse size %s", b.PartSize)
		}
		cfg.PartSize = size
	}
	if b.UploadConcurrency != 0 {
		cfg.UploadConcurrency = b.UploadConcurrency
	}
	if b.IncludeKeyring != nil {
		cfg.IncludeKeyring = *b.IncludeKeyring
	}
	if b.VerifyChecksum != nil {
		cfg.VerifyChecksum = *b.VerifyChecksum
	}
	if b.RequireApproval != nil {
		cfg.RequireApproval = *b.RequireApproval
	}

	encryption := func(e *BackupEncryptionConfig) *backup.EncryptionConfig {
		if e == nil {
			return nil
		}
		mode := e.Mode
		if mode == "" {
			mode = backup.EncryptionModeAge
		}
		return &backup.EncryptionConfig{Mode: mode, Recipients: slices.Clone(e.Recipients), KMSKey: e.KMSKey}
	}
	if b.Encryption != nil {
		cfg.Encryption = encryption(b.Encryption)
	}

	for _, d := range b.Destinations {
		dc := &backup.DestinationConfig{
			Name:            d.Name,
			Type:            d.Type,
			Path:            d.Path,
			Endpoint:        d.Endpoint,
			Bucket:          d.Bucket,
			Prefix:          d.Prefix,
			Region:          d.Region,
			ForcePathStyle:  d.ForcePathStyle != nil && *d.ForcePathStyle,
			AccessKey:       d.AccessKey,
			SecretKey:       d.SecretKey,
			SessionToken:    d.SessionToken,
			CredentialsFile: d.CredentialsFile,
			Profile:         d.Profile,
			CAFile:          d.CAFile,
			SSE:             d.SSE,
			KMSKey:          d.KMSKey,
			Encryption:      encryption(d.Encryption),
			Prune:           d.Prune == nil || *d.Prune,
		}
		if dc.Type == "" {
			dc.Type = backup.DestinationTypeS3
		}
		cfg.Destinations = append(cfg.Destinations, dc)
	}

	if r := b.Retention; r != nil {
		set := func(field *int, v *int) {
			if v != nil {
				*field = *v
			}
		}
		set(&cfg.Retention.KeepHourly, r.KeepHourly)
		set(&cfg.Retention.KeepDaily, r.KeepDaily)
		set(&cfg.Retention.KeepWeekly, r.KeepWeekly)
		set(&cfg.Retention.KeepMonthly, r.KeepMonthly)
		set(&cfg.Retention.MinKeep, r.MinKeep)
		set(&cfg.Retention.KeepFailed, r.KeepFailed)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid backup configuration: %w", err)
	}
	return cfg, nil
}
