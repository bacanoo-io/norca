// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/bacanoo-io/norca/norca/api/internal/testutil"
	"github.com/shoenig/test/must"
)

// The seals of a backup, which name what restores its secret store key, are
// decoded.
func TestBackup_Seals(t *testing.T) {
	testutil.Parallel(t)

	var backup Backup
	must.NoError(t, json.Unmarshal([]byte(`{"ID":"b1","Seals":[{"Type":"shares","KeyID":"k1","Threshold":2,"Shares":3}]}`), &backup))
	must.Eq(t, []*SealInfo{{Type: "shares", KeyID: "k1", Threshold: 2, Shares: 3}}, backup.Seals)
}
