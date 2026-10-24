// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/shoenig/test/must"
)

func TestConfig_Backup_Parse(t *testing.T) {
	ci.Parallel(t)

	id1, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	id2, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	hcl := `
server {
  enabled = true
  backup {
    enabled            = true
    interval           = "2h"
    jitter             = "10m"
    timeout            = "45m"
    retry_interval     = "5m"
    part_size          = "32MiB"
    upload_concurrency = 2
    include_keyring    = true
    verify_checksum    = true
    browse_cache_ttl   = "1m"

    destination "primary" {
      type    = "s3"
      bucket  = "company-norca-backup-eu"
      prefix  = "norca/${region}"
      region  = "eu-central-1"
      sse     = "aws:kms"
      kms_key = "arn:aws:kms:eu-central-1:111111111111:key/abcd"
    }

    destination "secondary" {
      type             = "s3"
      endpoint         = "https://s3.other-cloud.example.com"
      bucket           = "norca-backup-dr"
      force_path_style = true
      access_key       = "AKIA"
      secret_key       = "secret"
      prune            = false
      encryption {
        recipients = ["` + id2.Recipient().String() + `"]
      }
    }

    encryption {
      mode       = "age"
      recipients = ["` + id1.Recipient().String() + `"]
    }

    retention {
      keep_hourly  = 12
      keep_daily   = 7
      keep_weekly  = 0
      min_keep     = 5
    }
  }
}
`
	path := filepath.Join(t.TempDir(), "server.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))

	c, err := ParseConfigFile(path)
	must.NoError(t, err)
	must.NotNil(t, c.Server.Backup)

	cfg, err := c.Server.Backup.toServerConfig()
	must.NoError(t, err)
	must.True(t, cfg.Enabled)
	must.Eq(t, 2*time.Hour, cfg.Interval)
	must.Eq(t, 10*time.Minute, cfg.Jitter)
	must.Eq(t, 45*time.Minute, cfg.Timeout)
	must.Eq(t, 5*time.Minute, cfg.RetryInterval)
	must.Eq(t, uint64(32<<20), cfg.PartSize)
	must.Eq(t, 2, cfg.UploadConcurrency)
	must.True(t, cfg.VerifyChecksum)
	must.Eq(t, time.Minute, cfg.BrowseCacheTTL)
	must.Len(t, 2, cfg.Destinations)

	primary := cfg.Destination("primary")
	must.Eq(t, "norca/${region}", primary.Prefix)
	must.Eq(t, "aws:kms", primary.SSE)
	must.True(t, primary.Prune)
	must.Nil(t, primary.Encryption)

	secondary := cfg.Destination("secondary")
	must.True(t, secondary.ForcePathStyle)
	must.False(t, secondary.Prune)
	must.Eq(t, []string{id2.Recipient().String()}, secondary.Encryption.Recipients)
	must.Eq(t, []string{id2.Recipient().String()}, cfg.EncryptionFor(secondary).Recipients)
	must.Eq(t, []string{id1.Recipient().String()}, cfg.EncryptionFor(primary).Recipients)

	must.Eq(t, &backup.RetentionConfig{
		KeepHourly: 12, KeepDaily: 7, KeepWeekly: 0, KeepMonthly: 12, MinKeep: 5, KeepFailed: 10,
	}, cfg.Retention)

	// Secrets are redacted for the agent self endpoint.
	redacted := c.Server.Backup.Copy()
	redacted.Redact()
	must.Eq(t, "<redacted>", redacted.Destinations[1].SecretKey)
	must.Eq(t, "secret", c.Server.Backup.Destinations[1].SecretKey)
}

func TestConfig_Backup_ParseErrors(t *testing.T) {
	ci.Parallel(t)

	for name, hcl := range map[string]string{
		"unknown key":             `server { backup { bogus = 1 } }`,
		"unknown destination key": `server { backup { destination "a" { type = "s3" bucket = "b" bogus = 1 } } }`,
	} {
		path := filepath.Join(t.TempDir(), "server.hcl")
		must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))
		_, err := ParseConfigFile(path)
		must.ErrorContains(t, err, "bogus", must.Sprint(name))
	}

	for name, b := range map[string]*BackupConfig{
		"no destination": {Enabled: pointer.Of(true)},
		"bad duration":   {Interval: "soon"},
		"both":           {Interval: "1h", Schedule: "0 * * * *"},
		"no recipients": {Destinations: []*BackupDestinationConfig{
			{Name: "a", Bucket: "b"},
		}},
		"kms": {
			Destinations: []*BackupDestinationConfig{{Name: "a", Bucket: "b"}},
			Encryption:   &BackupEncryptionConfig{Mode: "kms", KMSKey: "k"},
		},
		"azure": {
			Destinations: []*BackupDestinationConfig{{Name: "a", Type: "azure", Bucket: "b"}},
		},
	} {
		_, err := b.toServerConfig()
		must.Error(t, err, must.Sprint(name))
	}
}

func TestConfig_Backup_Merge(t *testing.T) {
	ci.Parallel(t)

	a := &BackupConfig{
		Enabled:  pointer.Of(false),
		Interval: "1h",
		Destinations: []*BackupDestinationConfig{
			{Name: "primary", Bucket: "a"},
			{Name: "dr", Bucket: "dr"},
		},
		Encryption: &BackupEncryptionConfig{Recipients: []string{"age1a"}},
		Retention:  &BackupRetentionConfig{KeepHourly: pointer.Of(24), MinKeep: pointer.Of(3)},
	}
	b := &BackupConfig{
		Enabled:      pointer.Of(true),
		Destinations: []*BackupDestinationConfig{{Name: "primary", Bucket: "b"}},
		Encryption:   &BackupEncryptionConfig{Recipients: []string{"age1b"}},
		Retention:    &BackupRetentionConfig{MinKeep: pointer.Of(5)},
	}
	m := a.Merge(b)
	must.True(t, *m.Enabled)
	must.Eq(t, "1h", m.Interval)
	must.Len(t, 2, m.Destinations)
	must.Eq(t, "b", m.Destinations[0].Bucket)
	must.Eq(t, "dr", m.Destinations[1].Bucket)
	must.Eq(t, []string{"age1b"}, m.Encryption.Recipients)
	must.Eq(t, 24, *m.Retention.KeepHourly)
	must.Eq(t, 5, *m.Retention.MinKeep)

	// a is unchanged.
	must.False(t, *a.Enabled)
	must.Eq(t, "a", a.Destinations[0].Bucket)
	must.Eq(t, 3, *a.Retention.MinKeep)
}
