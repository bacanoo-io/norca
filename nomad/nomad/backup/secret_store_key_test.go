// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"bytes"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestSecretStoreKey_RoundTrip(t *testing.T) {
	ci.Parallel(t)

	key := bytes.Repeat([]byte{0x42}, 32)
	buf, err := EncodeSecretStoreKey(key)
	must.NoError(t, err)
	got, err := DecodeSecretStoreKey(bytes.NewReader(buf))
	must.NoError(t, err)
	must.Eq(t, key, got)

	_, err = DecodeSecretStoreKey(bytes.NewReader([]byte(`{"norca_secret_store_key":2,"core_key":"QkJC"}`)))
	must.ErrorContains(t, err, "invalid secret store key object")
	_, err = DecodeSecretStoreKey(bytes.NewReader([]byte(`[]`)))
	must.Error(t, err)

	objKey, err := ObjectKey(structs.BackupObjectSecretStoreKey)
	must.NoError(t, err)
	must.Eq(t, SecretStoreKeyKey, objKey)
}
