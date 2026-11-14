// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api/internal/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestKeyring_CRUD(t *testing.T) {
	testutil.Parallel(t)

	c, s := makeClient(t, nil, nil)
	defer s.Stop()

	kr := c.Keyring()

	// Find the bootstrap key
	keys, qm, err := kr.List(nil)
	must.NoError(t, err)
	assertQueryMeta(t, qm)
	must.Len(t, 1, keys)
	oldKeyID := keys[0].KeyID

	// Create a key by requesting a rotation
	key, wm, err := kr.Rotate(nil, nil)
	must.NoError(t, err)
	must.NotNil(t, key)
	assertWriteMeta(t, wm)

	// Read all the keys
	keys, qm, err = kr.List(&QueryOptions{WaitIndex: key.CreateIndex})
	must.NoError(t, err)
	assertQueryMeta(t, qm)
	must.Len(t, 2, keys)

	// Delete the old key. Norca: while the certificate authority of the
	// native mTLS still has its private key encrypted with the old key, the
	// removal is refused (409); the leader encrypts it with the active key
	// within the interval of its CA loop, then the removal goes.
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		wm, err = kr.Delete(&KeyringDeleteOptions{KeyID: oldKeyID}, nil)
		if err != nil && !(strings.Contains(err.Error(), "409") && strings.Contains(err.Error(), "native mTLS")) {
			t.Fatalf("unexpected error: %v", err)
		}
		return err
	}), wait.Timeout(90*time.Second), wait.Gap(time.Second)))
	assertWriteMeta(t, wm)

	// Read all the keys back
	keys, qm, err = kr.List(&QueryOptions{WaitIndex: key.CreateIndex})
	must.NoError(t, err)
	assertQueryMeta(t, qm)
	must.Len(t, 1, keys)
	must.Eq(t, key.KeyID, keys[0].KeyID)
	must.Eq(t, RootKeyState(RootKeyStateActive), keys[0].State)
}
