// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/consulkv"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// sealTest is a server with a bus and the bucket of the Consul KV, read
// both through the endpoint and as JetStream stores it.
type sealTest struct {
	*consulKVTest
	kv jetstream.KeyValue
}

func newSealTest(t *testing.T) *sealTest {
	srv, cleanup := TestServer(t, enableBus(t))
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, srv.RPC)
	waitForActiveRootKey(t, srv)
	c := &consulKVTest{t: t, srv: srv}
	c.waitBucket("")
	kv, err := busJS(t, srv).KeyValue(t.Context(), bus.ConsulKVBucket)
	must.NoError(t, err)
	return &sealTest{consulKVTest: c, kv: kv}
}

// stored returns the value of key as the bucket has it.
func (s *sealTest) stored(key string) []byte {
	s.t.Helper()
	e, err := s.kv.Get(s.t.Context(), testConsulKVKey(key))
	must.NoError(s.t, err)
	return e.Value()
}

// value returns the value of key as the endpoint gives it.
func (s *sealTest) value(key string) string {
	s.t.Helper()
	resp, err := s.get("", "default", key, false, 0)
	must.NoError(s.t, err)
	must.Len(s.t, 1, resp.Entries)
	return string(resp.Entries[0].Value)
}

func (s *sealTest) rotate(full bool) string {
	s.t.Helper()
	var resp structs.KeyringRotateRootKeyResponse
	must.NoError(s.t, s.srv.RPC("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{
		Full:         full,
		WriteRequest: structs.WriteRequest{Region: s.srv.config.Region},
	}, &resp))
	return resp.Key.KeyID
}

// TestConsulKVSeal_Stored checks what the bucket holds: a value written
// through the endpoint is sealed with the active root key and read back in
// the clear; a value stored in the clear before is read as it is and sealed
// by its next write; a sealed value does not open under another key.
func TestConsulKVSeal_Stored(t *testing.T) {
	ci.Parallel(t)
	s := newSealTest(t)
	active := waitForActiveRootKey(t, s.srv).KeyID

	const secret = "postgres://user:hunter2@db/app"
	ok, err := s.put("", "default", "app/db", secret, nil)
	must.NoError(t, err)
	must.True(t, ok)

	stored := s.stored("app/db")
	must.False(t, bytes.Contains(stored, []byte("hunter2")), must.Sprint("the bucket holds the value in the clear"))
	keyID, sealed := consulKVSealKeyID(stored)
	must.True(t, sealed)
	must.Eq(t, active, keyID)
	must.Eq(t, secret, s.value("app/db"))

	// The same value twice is not the same stored value (a nonce each).
	_, err = s.put("", "default", "app/db2", secret, nil)
	must.NoError(t, err)
	must.False(t, bytes.Equal(stored[len(consulKVSealMagic)+consulKVKeyIDLen:],
		s.stored("app/db2")[len(consulKVSealMagic)+consulKVKeyIDLen:]))

	// An empty value.
	_, err = s.put("", "default", "app/empty", "", nil)
	must.NoError(t, err)
	must.Eq(t, "", s.value("app/empty"))
	_, sealed = consulKVSealKeyID(s.stored("app/empty"))
	must.True(t, sealed)

	// Check-and-set works on the revisions as before.
	resp, err := s.get("", "default", "app/db", false, 0)
	must.NoError(t, err)
	ok, err = s.put("", "default", "app/db", "second", pointer.Of(resp.Entries[0].Revision))
	must.NoError(t, err)
	must.True(t, ok)
	ok, err = s.put("", "default", "app/db", "third", pointer.Of(resp.Entries[0].Revision))
	must.NoError(t, err)
	must.False(t, ok)
	must.Eq(t, "second", s.value("app/db"))

	// A value from before the bucket was sealed.
	_, err = s.kv.Put(t.Context(), testConsulKVKey("old/plain"), []byte("clear value"))
	must.NoError(t, err)
	must.Eq(t, "clear value", s.value("old/plain"))
	list, err := s.get("", "default", "", true, 0)
	must.NoError(t, err)
	must.Len(t, 4, list.Entries)
	_, err = s.put("", "default", "old/plain", "clear value", nil)
	must.NoError(t, err)
	_, sealed = consulKVSealKeyID(s.stored("old/plain"))
	must.True(t, sealed)
	must.Eq(t, "clear value", s.value("old/plain"))

	// A sealed value copied under another key in the store does not open
	// there, and is not handed out as it is.
	_, err = s.kv.Put(t.Context(), testConsulKVKey("public/copy"), s.stored("app/db"))
	must.NoError(t, err)
	_, err = s.get("", "default", "public/copy", false, 0)
	must.ErrorContains(t, err, "cannot be decrypted")
	// Nor one that was cut or changed.
	broken := bytes.Clone(s.stored("app/db"))
	broken[len(broken)-1] ^= 1
	_, err = s.kv.Put(t.Context(), testConsulKVKey("app/db"), broken)
	must.NoError(t, err)
	_, err = s.get("", "default", "app/db", false, 0)
	must.ErrorContains(t, err, "cannot be decrypted")
	_, err = s.kv.Put(t.Context(), testConsulKVKey("app/db"), broken[:len(consulKVSealMagic)+consulKVKeyIDLen+3])
	must.NoError(t, err)
	_, err = s.get("", "default", "app/db", false, 0)
	must.ErrorContains(t, err, "cannot be decrypted")
}

// TestConsulKVSeal_KeyInUse checks that a root key that seals a value of the
// bucket is not removed by the garbage collection of the keyring, and is
// once no value is sealed with it.
func TestConsulKVSeal_KeyInUse(t *testing.T) {
	ci.Parallel(t)
	s := newSealTest(t)
	// The first key of a server is in use by other things: the test has a
	// key of its own.
	oldKey := s.rotate(false)

	_, err := s.put("", "default", "kept", "sealed with the first key", nil)
	must.NoError(t, err)
	newKey := s.rotate(false)
	must.NotEq(t, oldKey, newKey)
	_, err = s.put("", "default", "newer", "sealed with the second key", nil)
	must.NoError(t, err)

	ids, err := s.srv.consulKVKeyIDs(t.Context())
	must.NoError(t, err)
	must.Eq(t, map[string]int{oldKey: 1, newKey: 1}, ids)

	gc := func() *structs.RootKeyMeta {
		t.Helper()
		store := s.srv.fsm.State()
		snap, err := store.Snapshot()
		must.NoError(t, err)
		c := NewCoreScheduler(s.srv, snap).(*CoreScheduler)
		must.NoError(t, c.rootKeyGC(s.srv.coreJobEval(structs.CoreJobForceGC, 0)))
		meta, err := store.RootKeyMetaByID(nil, oldKey)
		must.NoError(t, err)
		return meta
	}
	must.NotNil(t, gc(), must.Sprint("the key that seals a value of the Consul KV was removed"))

	// Nor by hand.
	remove := func() error {
		return s.srv.RPC("Keyring.Delete", &structs.KeyringDeleteRootKeyRequest{
			KeyID:        oldKey,
			WriteRequest: structs.WriteRequest{Region: s.srv.config.Region},
		}, &structs.KeyringDeleteRootKeyResponse{})
	}
	must.ErrorContains(t, remove(), "encrypts 1 value(s) of the Consul KV")
	meta, err := s.srv.fsm.State().RootKeyMetaByID(nil, oldKey)
	must.NoError(t, err)
	must.NotNil(t, meta)
	must.Eq(t, "sealed with the first key", s.value("kept"))

	// The value written again is sealed with the active key: the old key
	// seals nothing and goes.
	_, err = s.put("", "default", "kept", "sealed with the second key now", nil)
	must.NoError(t, err)
	must.Nil(t, gc())
	must.Eq(t, "sealed with the second key now", s.value("kept"))
}

// TestConsulKVSeal_Rekey checks the bucket's part of a full rotation of the
// keyring: the values sealed with the keys of before are sealed with the
// new key, and read the same.
func TestConsulKVSeal_Rekey(t *testing.T) {
	ci.Parallel(t)
	s := newSealTest(t)

	_, err := s.put("", "default", "a", "first", nil)
	must.NoError(t, err)
	s.rotate(false)
	_, err = s.put("", "default", "b", "second", nil)
	must.NoError(t, err)
	_, err = s.kv.Put(t.Context(), testConsulKVKey("plain"), []byte("clear"))
	must.NoError(t, err)

	newKey := s.rotate(true)
	waitBus(t, func() (bool, error) {
		ids, err := s.srv.consulKVKeyIDs(t.Context())
		return err == nil && len(ids) == 1 && ids[newKey] == 2, err
	})
	must.Eq(t, "first", s.value("a"))
	must.Eq(t, "second", s.value("b"))
	// A value in the clear is sealed by a write only.
	must.Eq(t, "clear", string(s.stored("plain")))
}

// TestConsulKVSeal_ExportImport checks that an export keeps the values
// sealed, that the summary of a key is the one of its value, and that an
// import seals with the active key what it gets sealed or in the clear.
func TestConsulKVSeal_ExportImport(t *testing.T) {
	ci.Parallel(t)
	s := newSealTest(t)
	oldKey := waitForActiveRootKey(t, s.srv).KeyID

	_, err := s.put("", "default", "app/config", "v1", nil)
	must.NoError(t, err)
	_, err = s.putPath("", "default", "scratch", "app/config", "other", nil)
	must.NoError(t, err)

	export, err := s.srv.exportConsulKV(t.Context())
	must.NoError(t, err)
	must.Len(t, 2, export.Entries)
	entry := export.Entry("default", consulkv.DefaultPath, "app/config")
	must.NotNil(t, entry)
	_, sealed := consulKVSealKeyID(entry.Value)
	must.True(t, sealed)

	sum := sha256.Sum256([]byte("v1"))
	stub := consulKVStub(entry, s.srv.encrypter)
	must.Eq(t, 2, stub.Size)
	must.Eq(t, hex.EncodeToString(sum[:]), stub.SHA256)
	// Without a keyring that opens it: the sealed value.
	must.NotEq(t, stub.SHA256, consulKVStub(entry).SHA256)

	// The export comes back after a rotation, with one more key in the
	// clear: all sealed with the new key, the key not in it removed.
	newKey := s.rotate(false)
	_, err = s.put("", "default", "later", "gone after the import", nil)
	must.NoError(t, err)
	entries := append(export.Entries, &consulkv.Entry{
		Namespace: "default", Path: consulkv.DefaultPath, Key: "clear", Value: []byte("from a file"),
	})
	must.NoError(t, s.srv.importConsulKV(t.Context(), entries, true))

	ids, err := s.srv.consulKVKeyIDs(t.Context())
	must.NoError(t, err)
	must.Eq(t, map[string]int{newKey: 3}, ids, must.Sprintf("the old key is %s", oldKey))
	must.Eq(t, "v1", s.value("app/config"))
	must.Eq(t, "from a file", s.value("clear"))
	resp, err := s.get("", "default", "later", false, 0)
	must.NoError(t, err)
	must.Len(t, 0, resp.Entries)

	// An entry sealed for another key of the bucket is refused, and nothing
	// of that import is written.
	moved := *entry
	moved.Key = "elsewhere"
	err = s.srv.importConsulKV(t.Context(), []*consulkv.Entry{&moved}, true)
	must.ErrorContains(t, err, "cannot be decrypted")
	must.Eq(t, "v1", s.value("app/config"))

}
