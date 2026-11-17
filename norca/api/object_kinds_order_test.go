// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/api/internal/testutil"
	"github.com/shoenig/test/must"
)

// The kinds come in the order of the server, which pages and reverses.
func TestObjectKinds_List_KeepsServerOrder(t *testing.T) {
	testutil.Parallel(t)
	c, _ := recordingServer(t, `[{"Group":"c.io","Kind":"Z"},{"Group":"b.io","Kind":"Y"},{"Group":"a.io","Kind":"X"}]`)

	kinds, _, err := c.ObjectKinds().List(&QueryOptions{Reverse: true})
	must.NoError(t, err)
	var refs []string
	for _, kind := range kinds {
		refs = append(refs, kind.KindRef())
	}
	must.Eq(t, []string{"c.io/Z", "b.io/Y", "a.io/X"}, refs)
}
