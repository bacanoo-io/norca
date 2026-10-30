// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

func TestEnsureStream(t *testing.T) {
	s := runNATS(t, true)
	js, err := jetstream.New(connect(t, s))
	must.NoError(t, err)
	ctx := context.Background()

	set := StreamSettings{
		Duplicates:  2 * time.Minute,
		Replicas:    1,
		MaxAge:      72 * time.Hour,
		MaxBytes:    64 << 20,
		Storage:     StorageFile,
		Compression: CompressionS2,
		AllowDirect: true,
		DenyDelete:  true,
	}
	res, err := EnsureStream(ctx, js, "EVENTS", "nomad", "r1", set)
	must.NoError(t, err)
	must.True(t, res.Created)

	info := streamConfig(t, js, "EVENTS")
	must.Eq(t, []string{"nomad.r1.>"}, info.Subjects)
	must.Eq(t, 2*time.Minute, info.Duplicates)
	must.Eq(t, 72*time.Hour, info.MaxAge)
	must.Eq(t, int64(64<<20), info.MaxBytes)
	must.Eq(t, jetstream.DiscardOld, info.Discard)
	must.Eq(t, jetstream.FileStorage, info.Storage)
	must.Eq(t, jetstream.S2Compression, info.Compression)
	must.True(t, info.AllowDirect)
	must.True(t, info.DenyDelete)
	must.False(t, info.DenyPurge)

	// Unchanged settings: nothing to do.
	res, err = EnsureStream(ctx, js, "EVENTS", "nomad", "r1", set)
	must.NoError(t, err)
	must.False(t, res.Created)
	must.SliceEmpty(t, res.Updated)
	must.SliceEmpty(t, res.Kept)

	// The limits, compression and direct gets follow the settings; storage
	// and the deny flags stay as created.
	set.MaxAge = 0
	set.MaxBytes = 0
	set.Compression = CompressionNone
	set.AllowDirect = false
	set.Storage = StorageMemory
	set.DenyDelete = false
	res, err = EnsureStream(ctx, js, "EVENTS", "nomad", "r1", set)
	must.NoError(t, err)
	must.Eq(t, []string{"max_age", "max_bytes", "compression", "allow_direct"}, res.Updated)
	must.Eq(t, []string{"storage", "deny_delete"}, res.Kept)

	info = streamConfig(t, js, "EVENTS")
	must.Eq(t, time.Duration(0), info.MaxAge)
	must.Eq(t, int64(-1), info.MaxBytes)
	must.Eq(t, jetstream.NoCompression, info.Compression)
	must.False(t, info.AllowDirect)
	must.Eq(t, jetstream.FileStorage, info.Storage)
	must.True(t, info.DenyDelete)

	// A memory stream is created without compression.
	res, err = EnsureStream(ctx, js, "MEM", "nomad", "r2", StreamSettings{Duplicates: time.Minute, Storage: StorageMemory, MaxAge: time.Hour})
	must.NoError(t, err)
	must.True(t, res.Created)
	info = streamConfig(t, js, "MEM")
	must.Eq(t, jetstream.MemoryStorage, info.Storage)
	must.Eq(t, jetstream.NoCompression, info.Compression)
	must.Eq(t, 1, info.Replicas)
}

func streamConfig(t *testing.T, js jetstream.JetStream, name string) jetstream.StreamConfig {
	t.Helper()
	s, err := js.Stream(context.Background(), name)
	must.NoError(t, err)
	info, err := s.Info(context.Background())
	must.NoError(t, err)
	return info.Config
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"":        0,
		"0":       0,
		"1048576": 1 << 20,
		"512MB":   512 << 20,
		"512mb":   512 << 20,
		"1GiB":    1 << 30,
		"2G":      2 << 30,
		"64K":     64 << 10,
		"1TB":     1 << 40,
	} {
		got, err := ParseBytes(in)
		must.NoError(t, err, must.Sprint(in))
		must.Eq(t, want, got, must.Sprint(in))
	}
	for _, in := range []string{"lots", "-1", "1.5GB", "MB", "99999999999T"} {
		_, err := ParseBytes(in)
		must.Error(t, err, must.Sprint(in))
	}
}
