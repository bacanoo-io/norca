// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import "testing"

// A mirror is fetched once in a run, and a run of a host that serves
// requests is a request: the next one sees what was tagged since.
func TestFetcher_RunIsARequest(t *testing.T) {
	t.Parallel()

	host := New(t.TempDir())
	first := host.WithGlobalRegistry(t.TempDir())
	mirror := first.mirrorDir("https://example.com/acme/modules")

	first.setUpdated(mirror)

	if !first.isUpdated(mirror) {
		t.Error("a run fetches a mirror twice")
	}

	if host.WithGlobalRegistry(t.TempDir()).isUpdated(mirror) {
		t.Error("the next request takes the mirror of an earlier one as fetched")
	}
}
