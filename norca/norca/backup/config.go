// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package backup implements the scheduled backup of a region's Raft state and
// keyring into S3-compatible stores and directories. The Manager runs on the
// leader; the server's Backup RPC endpoint uses it to browse and restore
// backups.
package backup

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/cronexpr"
	"github.com/hashicorp/go-multierror"
)

const (
	// DestinationTypeS3 is AWS S3 or an S3-compatible store such as MinIO.
	DestinationTypeS3 = "s3"

	// DestinationTypeDirectory is a directory of the server's filesystem.
	// It must be the same shared storage on every server of the region, or
	// the region must have a single server.
	DestinationTypeDirectory = "directory"

	// EncryptionModeAge encrypts with age public keys (recipients); the
	// private key never reaches the cluster.
	EncryptionModeAge = "age"

	// EncryptionModeKMS is the envelope encryption with a cloud KMS. It is
	// reserved; see KeyWrapper.
	EncryptionModeKMS = "kms"

	// DefaultPrefix is the object prefix of a destination that sets none.
	DefaultPrefix = "norca/${region}"
)

// Defaults, tuned for snapshots of up to about 2 GB.
const (
	DefaultInterval          = time.Hour
	DefaultJitter            = 5 * time.Minute
	DefaultTimeout           = time.Hour
	DefaultRetryInterval     = 10 * time.Minute
	DefaultPartSize          = 16 << 20 // 16 MiB: 128 parts for 2 GB, 160 GB at most
	DefaultUploadConcurrency = 4
	DefaultBrowseCacheTTL    = 5 * time.Minute
	DefaultApprovalTTL       = time.Hour

	// minPartSize is the S3 minimum part size of a multipart upload.
	minPartSize = 5 << 20
	// maxPartSize is the S3 maximum part size.
	maxPartSize = 5 << 30
)

// Config is the server's backup configuration.
type Config struct {
	// Enabled starts the scheduled backups on the leader. Manual backups,
	// browsing and restore only need destinations and encryption.
	Enabled bool

	// Interval or Schedule (a cron expression, UTC) sets the scheduled
	// times. Interval slots are aligned to the Unix epoch, shifted by the
	// region's jitter offset.
	Interval time.Duration
	Schedule string

	// Jitter is the upper bound of a per-region offset of the scheduled
	// times, so regions sharing a store don't upload at the same time.
	Jitter time.Duration

	// Timeout bounds a whole backup run.
	Timeout time.Duration

	// RetryInterval is the wait after a failed scheduled backup.
	RetryInterval time.Duration

	// PartSize is the multipart upload part size; UploadConcurrency parts
	// are buffered and uploaded in parallel (memory use is their product).
	PartSize          uint64
	UploadConcurrency int

	// IncludeKeyring adds the encrypted keyring (the keys of Variables and
	// workload identities) to every backup.
	IncludeKeyring bool

	// VerifyChecksum downloads every uploaded object again and compares its
	// SHA-256. Without it only the size is verified.
	VerifyChecksum bool

	// BrowseCacheTTL is how long a decrypted backup loaded for browsing
	// stays in memory; 0 disables the cache.
	BrowseCacheTTL time.Duration

	// RequireApproval makes a second administrator approve every full
	// restore. Two-person approval is mandatory for full restores, so it
	// can't be turned off.
	RequireApproval bool

	// ApprovalTTL is how long a full restore request waits for approval.
	ApprovalTTL time.Duration

	Destinations []*DestinationConfig
	Encryption   *EncryptionConfig
	Retention    *RetentionConfig
}

// DestinationConfig is an S3-compatible store or a directory.
type DestinationConfig struct {
	Name string
	Type string

	// Path is the absolute path of a directory destination.
	Path string

	// Prefix is the folder of the region's backups in the bucket or below
	// the path; ${region} is replaced by the region.
	Prefix string

	// The remaining fields up to Encryption are those of S3 destinations.

	// Endpoint is host[:port] or a URL; an http:// URL disables TLS. Empty
	// means AWS S3.
	Endpoint       string
	Bucket         string
	Region         string
	ForcePathStyle bool

	// Credentials: static keys, an AWS shared credentials file (with an
	// optional profile), or else the environment and the instance profile.
	AccessKey       string
	SecretKey       string
	SessionToken    string
	CredentialsFile string
	Profile         string

	// CAFile is a PEM bundle of CAs to trust for the endpoint.
	CAFile string

	// SSE is the server-side encryption: "", "AES256" or "aws:kms" (with
	// KMSKey).
	SSE    string
	KMSKey string

	// Encryption overrides the client-side encryption for this destination,
	// for example a disaster recovery copy for other recipients.
	Encryption *EncryptionConfig

	// Prune lets retention delete expired backups on this destination. Turn
	// it off where the server may only write and the store's lifecycle rules
	// expire objects.
	Prune bool
}

// EncryptionConfig is the client-side encryption of backup objects.
type EncryptionConfig struct {
	// Mode is "age" (the default). "kms" is reserved.
	Mode string

	// Recipients are age public keys (age1...). The private keys are kept
	// by the administrators outside the cluster.
	Recipients []string

	// KMSKey is reserved for the kms mode.
	KMSKey string
}

// RetentionConfig is the grandfather-father-son retention of backups.
type RetentionConfig struct {
	KeepHourly  int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int

	// MinKeep backups (the newest ones) are never deleted.
	MinKeep int

	// KeepFailed is the number of failed backup records kept.
	KeepFailed int
}

// DefaultConfig returns the default (disabled) configuration.
func DefaultConfig() *Config {
	return &Config{
		Interval:          DefaultInterval,
		Jitter:            DefaultJitter,
		Timeout:           DefaultTimeout,
		RetryInterval:     DefaultRetryInterval,
		PartSize:          DefaultPartSize,
		UploadConcurrency: DefaultUploadConcurrency,
		IncludeKeyring:    true,
		BrowseCacheTTL:    DefaultBrowseCacheTTL,
		RequireApproval:   true,
		ApprovalTTL:       DefaultApprovalTTL,
		Encryption:        &EncryptionConfig{Mode: EncryptionModeAge},
		Retention:         DefaultRetention(),
	}
}

// DefaultRetention returns the default retention.
func DefaultRetention() *RetentionConfig {
	return &RetentionConfig{
		KeepHourly:  24,
		KeepDaily:   14,
		KeepWeekly:  8,
		KeepMonthly: 12,
		MinKeep:     3,
		KeepFailed:  10,
	}
}

// Configured reports whether backups can be taken and read: at least one
// destination exists.
func (c *Config) Configured() bool {
	return c != nil && len(c.Destinations) > 0
}

// Copy returns a deep copy of the configuration.
func (c *Config) Copy() *Config {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Destinations = nil
	for _, d := range c.Destinations {
		nd := *d
		nd.Encryption = d.Encryption.Copy()
		nc.Destinations = append(nc.Destinations, &nd)
	}
	nc.Encryption = c.Encryption.Copy()
	if c.Retention != nil {
		r := *c.Retention
		nc.Retention = &r
	}
	return &nc
}

// Copy returns a copy of the encryption configuration.
func (e *EncryptionConfig) Copy() *EncryptionConfig {
	if e == nil {
		return nil
	}
	ne := *e
	ne.Recipients = slices.Clone(e.Recipients)
	return &ne
}

// Destination returns the named destination or nil.
func (c *Config) Destination(name string) *DestinationConfig {
	for _, d := range c.Destinations {
		if d.Name == name {
			return d
		}
	}
	return nil
}

// DestinationNames returns the names of the destinations.
func (c *Config) DestinationNames() []string {
	names := make([]string, 0, len(c.Destinations))
	for _, d := range c.Destinations {
		names = append(names, d.Name)
	}
	return names
}

// EncryptionFor returns the effective encryption of a destination.
func (c *Config) EncryptionFor(d *DestinationConfig) *EncryptionConfig {
	if d != nil && d.Encryption != nil {
		return d.Encryption
	}
	return c.Encryption
}

// Validate returns an error if the configuration is invalid.
func (c *Config) Validate() error {
	var mErr *multierror.Error
	add := func(format string, args ...any) {
		mErr = multierror.Append(mErr, fmt.Errorf(format, args...))
	}

	if c.Enabled && len(c.Destinations) == 0 {
		add("at least one destination is required when backups are enabled")
	}
	if c.Schedule != "" {
		if _, err := cronexpr.Parse(c.Schedule); err != nil {
			add("invalid schedule %q: %v", c.Schedule, err)
		}
	} else if c.Interval < time.Minute {
		add("interval must be at least 1m")
	}
	if c.Jitter < 0 {
		add("jitter must not be negative")
	}
	if c.Timeout <= 0 {
		add("timeout must be positive")
	}
	if c.RetryInterval <= 0 {
		add("retry_interval must be positive")
	}
	if c.PartSize < minPartSize || c.PartSize > maxPartSize {
		add("part_size must be between 5MiB and 5GiB")
	}
	if c.UploadConcurrency < 1 || c.UploadConcurrency > 64 {
		add("upload_concurrency must be between 1 and 64")
	}
	if c.BrowseCacheTTL < 0 {
		add("browse_cache_ttl must not be negative")
	}
	if !c.RequireApproval {
		add("require_approval can't be disabled: two-person approval is mandatory for full restores")
	}
	if c.ApprovalTTL < time.Minute || c.ApprovalTTL > 7*24*time.Hour {
		add("approval_ttl must be between 1m and 168h")
	}

	seen := make(map[string]struct{})
	for _, d := range c.Destinations {
		if _, ok := seen[d.Name]; ok {
			add("duplicate destination %q", d.Name)
		}
		seen[d.Name] = struct{}{}
		if err := d.Validate(); err != nil {
			add("destination %q: %v", d.Name, err)
		}
		if len(c.Destinations) > 0 {
			if err := c.EncryptionFor(d).Validate(); err != nil {
				add("destination %q: encryption: %v", d.Name, err)
			}
		}
	}

	if r := c.Retention; r != nil {
		for name, v := range map[string]int{
			"keep_hourly": r.KeepHourly, "keep_daily": r.KeepDaily,
			"keep_weekly": r.KeepWeekly, "keep_monthly": r.KeepMonthly,
			"min_keep": r.MinKeep, "keep_failed": r.KeepFailed,
		} {
			if v < 0 {
				add("retention %s must not be negative", name)
			}
		}
	}
	return mErr.ErrorOrNil()
}

// Validate returns an error if the destination is invalid.
func (d *DestinationConfig) Validate() error {
	var mErr *multierror.Error
	add := func(format string, args ...any) {
		mErr = multierror.Append(mErr, fmt.Errorf(format, args...))
	}
	if d.Name == "" {
		add("missing name")
	}
	switch d.Type {
	case DestinationTypeS3:
		if d.Path != "" {
			add("path is only supported by type %q", DestinationTypeDirectory)
		}
		if d.Bucket == "" {
			add("missing bucket")
		}
		if (d.AccessKey == "") != (d.SecretKey == "") {
			add("access_key and secret_key must be set together")
		}
		if d.AccessKey != "" && d.CredentialsFile != "" {
			add("access_key and credentials_file are mutually exclusive")
		}
		switch d.SSE {
		case "", "AES256":
		case "aws:kms":
		default:
			add("invalid sse %q, must be \"AES256\" or \"aws:kms\"", d.SSE)
		}
		if d.KMSKey != "" && d.SSE != "aws:kms" {
			add("kms_key requires sse = \"aws:kms\"")
		}
	case DestinationTypeDirectory:
		if d.Path == "" {
			add("missing path")
		} else if !filepath.IsAbs(d.Path) {
			add("path %q must be absolute", d.Path)
		}
		if hasDotDot(d.Prefix) {
			add("prefix must not contain \"..\"")
		}
		for _, key := range d.s3Keys() {
			add("%s is not supported by type %q", key, DestinationTypeDirectory)
		}
	default:
		add("unsupported type %q, must be %q or %q", d.Type, DestinationTypeS3, DestinationTypeDirectory)
	}
	if strings.Contains(strings.ReplaceAll(d.Prefix, "${region}", ""), "${") {
		add("prefix may only interpolate ${region}")
	}
	return mErr.ErrorOrNil()
}

// s3Keys returns the configuration keys of the S3 options that are set.
func (d *DestinationConfig) s3Keys() []string {
	var keys []string
	for _, o := range []struct {
		key string
		set bool
	}{
		{"endpoint", d.Endpoint != ""},
		{"bucket", d.Bucket != ""},
		{"region", d.Region != ""},
		{"force_path_style", d.ForcePathStyle},
		{"access_key", d.AccessKey != ""},
		{"secret_key", d.SecretKey != ""},
		{"session_token", d.SessionToken != ""},
		{"credentials_file", d.CredentialsFile != ""},
		{"profile", d.Profile != ""},
		{"ca_file", d.CAFile != ""},
		{"sse", d.SSE != ""},
		{"kms_key", d.KMSKey != ""},
	} {
		if o.set {
			keys = append(keys, o.key)
		}
	}
	return keys
}

// errKMSNotImplemented is returned for the reserved kms mode.
var errKMSNotImplemented = errors.New(`encryption mode "kms" is not implemented yet, use "age"`)

// Validate returns an error if the encryption is invalid. Client-side
// encryption is mandatory: the keyring holds key material.
func (e *EncryptionConfig) Validate() error {
	if e == nil {
		return errors.New("missing encryption block")
	}
	switch e.Mode {
	case "", EncryptionModeAge:
		if len(e.Recipients) == 0 {
			return errors.New("at least one age recipient is required")
		}
		if _, err := parseRecipients(e.Recipients); err != nil {
			return err
		}
		return nil
	case EncryptionModeKMS:
		return errKMSNotImplemented
	default:
		return fmt.Errorf("invalid mode %q", e.Mode)
	}
}
