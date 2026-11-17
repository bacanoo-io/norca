// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/consulkv"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/shoenig/test/must"
)

// TestConsulKV_ChecksumVerified replaces the Consul KV export of a backup
// with another one encrypted for the same public recipient, as anyone who
// can write the destination can. Both readers of the export must refuse it:
// the checksum recorded in Raft does not match.
func TestConsulKV_ChecksumVerified(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, 2048)
	genuine, err := consulkv.Encode(&consulkv.Export{Bucket: "consul-kv", Entries: []*consulkv.Entry{
		{Namespace: "default", Path: "app", Key: "app/config", Value: []byte("genuine")},
	}})
	must.NoError(t, err)
	env.deps.ConsulKV = func(context.Context) ([]byte, error) { return genuine, nil }

	cfg, dir := env.dirTestConfig(t)
	cfg.Destinations = cfg.Destinations[:1]
	m := New(cfg, env.deps)
	m.SetLeader(true)
	defer m.SetLeader(false)
	record, err := m.Save(SaveOptions{})
	must.NoError(t, err)

	dest, st, err := m.ReadDestination(record, "local")
	must.NoError(t, err)
	restore := func(archive io.Reader) (*state.StateStore, error) {
		_, err := io.ReadAll(archive)
		return state.TestStateStore(t), err
	}

	// The untouched backup is read by both paths.
	export, err := ReadConsulKV(t.Context(), dest, record, st, env.identities(t))
	must.NoError(t, err)
	must.Len(t, 1, export.Entries)
	must.Eq(t, "genuine", string(export.Entries[0].Value))

	// The forged export: no keys at all, which a full restore would turn
	// into the deletion of every key.
	forged, err := consulkv.Encode(&consulkv.Export{Bucket: "consul-kv"})
	must.NoError(t, err)
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, env.identity.Recipient())
	must.NoError(t, err)
	_, err = w.Write(forged)
	must.NoError(t, err)
	must.NoError(t, w.Close())
	object := filepath.Join(dir, "norca", "eu", filepath.FromSlash(record.Path), ConsulKVKey)
	_, err = os.Stat(object)
	must.NoError(t, err)
	must.NoError(t, os.WriteFile(object, sealed.Bytes(), 0o600))

	// Full restore.
	export, err = ReadConsulKV(t.Context(), dest, record, st, env.identities(t))
	must.Error(t, err, must.Sprint("the full restore accepted a replaced Consul KV export"))
	must.StrContains(t, err.Error(), "mismatch")
	must.Nil(t, export)

	// Browse and selective restore.
	loaded, err := m.Load(t.Context(), record, "local", env.identity.String(), restore)
	must.Error(t, err, must.Sprint("the load accepted a replaced Consul KV export"))
	must.StrContains(t, err.Error(), "mismatch")
	must.Nil(t, loaded)
}
