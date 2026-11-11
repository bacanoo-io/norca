// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// observerManager returns a manager of the same region, store and
// destinations as the leader's in env, named obs-1: the observer.
func observerManager(env *testEnv, cfg *Config) *Manager {
	deps := *env.deps
	deps.ServerName = "obs-1"
	deps.Delegate = nil
	deps.Apply = func(structs.MessageType, any) (uint64, error) {
		panic("an observer must not write to Raft")
	}
	return New(cfg, &deps)
}

func TestConfig_Fingerprint(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, 10)

	a, b := env.config("primary", "dr"), env.config("dr", "primary")
	must.Eq(t, a.Fingerprint(), b.Fingerprint())

	// Credentials and the scheduling do not count.
	b.Destinations[0].AccessKey = "other"
	b.Destinations[0].SecretKey = "other"
	b.Interval = 0
	b.PreferObserver = true
	must.Eq(t, a.Fingerprint(), b.Fingerprint())

	// Where and how a backup is stored do.
	for name, change := range map[string]func(*Config){
		"recipient":       func(c *Config) { c.Encryption.Recipients = []string{"age1other"} },
		"bucket":          func(c *Config) { c.Destinations[0].Bucket = "elsewhere" },
		"prefix":          func(c *Config) { c.Destinations[0].Prefix = "x/${region}" },
		"keyring":         func(c *Config) { c.IncludeKeyring = !c.IncludeKeyring },
		"destination set": func(c *Config) { c.Destinations = c.Destinations[:1] },
	} {
		c := env.config("primary", "dr")
		change(c)
		must.NotEq(t, a.Fingerprint(), c.Fingerprint(), must.Sprint(name))
	}
}

func TestManager_Delegate(t *testing.T) {
	ci.Parallel(t)

	setup := func(t *testing.T) (*testEnv, *Manager, *Manager) {
		env := newTestEnv(t, 1000)
		cfg := env.config("primary")
		cfg.PreferObserver = true
		obs := observerManager(env, env.config("primary"))
		env.deps.Delegate = func(ctx context.Context, req *DelegateRequest) (*structs.BackupRecord, error) {
			return obs.TakeDelegated(ctx, req)
		}
		m := New(cfg, env.deps)
		m.SetLeader(true)
		t.Cleanup(func() { m.SetLeader(false) })
		return env, m, obs
	}

	t.Run("the observer takes it, the leader records it", func(t *testing.T) {
		env, m, _ := setup(t)
		var started string
		record, err := m.Save(SaveOptions{Labels: map[string]string{"a": "b"}, Started: func(id string) { started = id }})
		must.NoError(t, err)
		must.Eq(t, started, record.ID)
		must.Eq(t, "obs-1", record.TakenBy)
		must.Eq(t, structs.BackupStatusComplete, record.Status)
		must.Eq(t, map[string]string{"a": "b"}, record.Labels)

		stored, err := env.store.BackupByID(nil, record.ID)
		must.NoError(t, err)
		must.Eq(t, "obs-1", stored.TakenBy)

		var manifest Manifest
		must.NoError(t, json.Unmarshal(env.s3.Object("bucket-primary", "norca/eu/"+record.Path+"/"+ManifestKey), &manifest))
		must.Eq(t, "obs-1", manifest.TakenBy)
	})

	t.Run("the pre-restore backup stays on the leader", func(t *testing.T) {
		_, m, _ := setup(t)
		record, err := m.Save(SaveOptions{Trigger: structs.BackupTriggerPreRestore})
		must.NoError(t, err)
		must.Eq(t, "srv-1", record.TakenBy)
	})

	t.Run("a refusal: the leader takes it with the same ID", func(t *testing.T) {
		env, m, _ := setup(t)
		env.deps.Delegate = func(context.Context, *DelegateRequest) (*structs.BackupRecord, error) {
			return nil, fmt.Errorf("%w: no healthy observer", ErrDelegateRefused)
		}
		var started string
		record, err := m.Save(SaveOptions{Started: func(id string) { started = id }})
		must.NoError(t, err)
		must.Eq(t, started, record.ID)
		must.Eq(t, "srv-1", record.TakenBy)
	})

	t.Run("a different configuration on the observer is a refusal", func(t *testing.T) {
		env, m, _ := setup(t)
		other := env.config("primary")
		other.Destinations[0].Bucket = "bucket-other"
		obs := observerManager(env, other)
		env.deps.Delegate = func(ctx context.Context, req *DelegateRequest) (*structs.BackupRecord, error) {
			return obs.TakeDelegated(ctx, req)
		}
		_, err := obs.TakeDelegated(context.Background(), &DelegateRequest{ID: NewID(time.Now(), 1), Fingerprint: m.Config().Fingerprint()})
		must.ErrorIs(t, err, ErrDelegateRefused)

		record, err := m.Save(SaveOptions{})
		must.NoError(t, err)
		must.Eq(t, "srv-1", record.TakenBy)
		must.SliceEmpty(t, env.s3.Keys("bucket-other"))
	})

	t.Run("a failed observer backup: the leader takes it with the same ID", func(t *testing.T) {
		env, m, obs := setup(t)
		env.deps.Delegate = func(ctx context.Context, req *DelegateRequest) (*structs.BackupRecord, error) {
			rec, err := obs.TakeDelegated(ctx, req)
			if err != nil {
				return nil, err
			}
			rec.Status = structs.BackupStatusFailed
			rec.StatusDescription = "upload failed"
			return rec, nil
		}
		var started string
		record, err := m.Save(SaveOptions{Started: func(id string) { started = id }})
		must.NoError(t, err)
		must.Eq(t, started, record.ID)
		must.Eq(t, "srv-1", record.TakenBy)
	})

	t.Run("no answer: recorded as failed, the leader takes a new one", func(t *testing.T) {
		env, m, _ := setup(t)
		env.deps.Delegate = func(context.Context, *DelegateRequest) (*structs.BackupRecord, error) {
			return nil, errors.New("connection reset")
		}
		var started string
		record, err := m.Save(SaveOptions{Started: func(id string) { started = id }})
		must.NoError(t, err)
		must.NotEq(t, started, record.ID)
		must.Eq(t, "srv-1", record.TakenBy)

		failed, err := env.store.BackupByID(nil, started)
		must.NoError(t, err)
		must.NotNil(t, failed)
		must.Eq(t, structs.BackupStatusFailed, failed.Status)
		must.StrContains(t, failed.StatusDescription, "connection reset")
	})

	t.Run("a record of another backup is not accepted", func(t *testing.T) {
		env, m, obs := setup(t)
		env.deps.Delegate = func(ctx context.Context, req *DelegateRequest) (*structs.BackupRecord, error) {
			rec, err := obs.TakeDelegated(ctx, req)
			if err == nil {
				rec.Region = "other"
			}
			return rec, err
		}
		record, err := m.Save(SaveOptions{})
		must.NoError(t, err)
		must.Eq(t, "srv-1", record.TakenBy)
	})
}
